package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pod32g/omni-notify/internal/models"
)

// UpsertProvider inserts or updates a provider, encrypting its secret at rest.
// An empty Secret leaves the stored secret untouched on update (so callers can
// edit config without re-supplying the secret) and stores NULL on insert.
func (s *Store) UpsertProvider(ctx context.Context, p models.ProviderConfig) error {
	if p.ManagedBy == "" {
		p.ManagedBy = models.ManagedByAPI
	}
	cfgJSON, err := json.Marshal(orEmptyAny(p.Config))
	if err != nil {
		return fmt.Errorf("marshal provider config: %w", err)
	}
	now := s.now()

	// Determine whether the provider already exists so we can preserve the stored
	// secret (when none is supplied) and the original created_at on update.
	var existing int
	err = s.db.QueryRowContext(ctx, `SELECT 1 FROM providers WHERE name = ?`, p.Name).Scan(&existing)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("lookup provider: %w", err)
	}

	if exists {
		if p.Secret == "" {
			_, err = s.db.ExecContext(ctx, `
				UPDATE providers SET kind=?, config=?, enabled=?, managed_by=?, updated_at=?
				WHERE name=?`,
				p.Kind, string(cfgJSON), boolToInt(p.Enabled), string(p.ManagedBy), fmtTime(now), p.Name)
		} else {
			enc, encErr := s.cipher.Encrypt(p.Secret)
			if encErr != nil {
				return encErr
			}
			_, err = s.db.ExecContext(ctx, `
				UPDATE providers SET kind=?, config=?, secret=?, enabled=?, managed_by=?, updated_at=?
				WHERE name=?`,
				p.Kind, string(cfgJSON), enc, boolToInt(p.Enabled), string(p.ManagedBy), fmtTime(now), p.Name)
		}
		if err != nil {
			return fmt.Errorf("update provider: %w", err)
		}
		return nil
	}

	var enc []byte
	if p.Secret != "" {
		enc, err = s.cipher.Encrypt(p.Secret)
		if err != nil {
			return err
		}
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO providers (name, kind, config, secret, enabled, managed_by, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		p.Name, p.Kind, string(cfgJSON), enc, boolToInt(p.Enabled), string(p.ManagedBy), fmtTime(now), fmtTime(now))
	if err != nil {
		return fmt.Errorf("insert provider: %w", err)
	}
	return nil
}

// ProviderExists reports whether a provider with the given name exists.
func (s *Store) ProviderExists(ctx context.Context, name string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM providers WHERE name = ?`, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("provider exists: %w", err)
	}
	return true, nil
}

// GetProvider returns a provider with its secret decrypted (for sending).
func (s *Store) GetProvider(ctx context.Context, name string) (models.ProviderConfig, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT name, kind, config, secret, enabled, managed_by, created_at, updated_at
		FROM providers WHERE name = ?`, name)
	p, err := s.scanProvider(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return models.ProviderConfig{}, ErrNotFound
	}
	return p, err
}

// ListProviders returns all providers. Secrets are decrypted so the notifier can
// use them; API responses must rely on the json:"-" tag to keep them hidden.
func (s *Store) ListProviders(ctx context.Context) ([]models.ProviderConfig, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, kind, config, secret, enabled, managed_by, created_at, updated_at
		FROM providers ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	defer rows.Close()
	var out []models.ProviderConfig
	for rows.Next() {
		p, err := s.scanProvider(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// HasEncryptedSecrets reports whether any provider has a stored secret. Used at
// startup to fail fast when secrets exist but no encryption key is configured.
func (s *Store) HasEncryptedSecrets(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM providers WHERE secret IS NOT NULL AND length(secret) > 0`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("count secrets: %w", err)
	}
	return n > 0, nil
}

func (s *Store) scanProvider(sc scanner, decrypt bool) (models.ProviderConfig, error) {
	var (
		p                      models.ProviderConfig
		cfgJSON                string
		secret                 []byte
		enabled                int
		managedBy              string
		createdStr, updatedStr string
	)
	if err := sc.Scan(&p.Name, &p.Kind, &cfgJSON, &secret, &enabled, &managedBy, &createdStr, &updatedStr); err != nil {
		return models.ProviderConfig{}, err
	}
	if cfgJSON != "" && cfgJSON != "{}" {
		_ = json.Unmarshal([]byte(cfgJSON), &p.Config)
	}
	p.Enabled = enabled != 0
	p.ManagedBy = models.ManagedBy(managedBy)
	p.CreatedAt = parseTime(createdStr)
	p.UpdatedAt = parseTime(updatedStr)
	if decrypt && len(secret) > 0 {
		plain, err := s.cipher.Decrypt(secret)
		if err != nil {
			return models.ProviderConfig{}, fmt.Errorf("provider %q: %w", p.Name, err)
		}
		p.Secret = plain
	}
	return p, nil
}

func orEmptyAny(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
