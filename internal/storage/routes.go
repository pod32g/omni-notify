package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pod32g/omni-notify/internal/models"
)

// UpsertRoute inserts or updates a route, preserving created_at on update.
func (s *Store) UpsertRoute(ctx context.Context, r models.Route) error {
	if r.ManagedBy == "" {
		r.ManagedBy = models.ManagedByAPI
	}
	matchJSON, err := json.Marshal(orEmptyStrMap(r.Match))
	if err != nil {
		return fmt.Errorf("marshal route match: %w", err)
	}
	provJSON, err := json.Marshal(orEmptySlice(r.Providers))
	if err != nil {
		return fmt.Errorf("marshal route providers: %w", err)
	}
	now := s.now()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO routes (name, match, providers, is_default, disabled, priority,
		                    stop_processing, dedup_window, repeat_interval, managed_by,
		                    created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET
			match           = excluded.match,
			providers       = excluded.providers,
			is_default      = excluded.is_default,
			disabled        = excluded.disabled,
			priority        = excluded.priority,
			stop_processing = excluded.stop_processing,
			dedup_window    = excluded.dedup_window,
			repeat_interval = excluded.repeat_interval,
			managed_by      = excluded.managed_by,
			updated_at      = excluded.updated_at`,
		r.Name, string(matchJSON), string(provJSON), boolToInt(r.IsDefault), boolToInt(r.Disabled),
		r.Priority, boolToInt(r.StopProcessing), int64(r.DedupWindow.D()), int64(r.RepeatInterval.D()),
		string(r.ManagedBy), fmtTime(now), fmtTime(now))
	if err != nil {
		return fmt.Errorf("upsert route: %w", err)
	}
	return nil
}

// RouteExists reports whether a route with the given name exists.
func (s *Store) RouteExists(ctx context.Context, name string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM routes WHERE name = ?`, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("route exists: %w", err)
	}
	return true, nil
}

// GetRoute returns a route by name.
func (s *Store) GetRoute(ctx context.Context, name string) (models.Route, error) {
	row := s.db.QueryRowContext(ctx, selectRouteCols+` FROM routes WHERE name = ?`, name)
	r, err := scanRoute(row)
	if errors.Is(err, sql.ErrNoRows) {
		return models.Route{}, ErrNotFound
	}
	return r, err
}

// ListRoutes returns all routes ordered by name.
func (s *Store) ListRoutes(ctx context.Context) ([]models.Route, error) {
	rows, err := s.db.QueryContext(ctx, selectRouteCols+` FROM routes ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list routes: %w", err)
	}
	defer rows.Close()
	var out []models.Route
	for rows.Next() {
		r, err := scanRoute(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const selectRouteCols = `SELECT name, match, providers, is_default, disabled, priority,
	stop_processing, dedup_window, repeat_interval, managed_by, created_at, updated_at`

func scanRoute(sc scanner) (models.Route, error) {
	var (
		r                      models.Route
		matchJSON, provJSON    string
		isDefault, disabled    int
		priority               int
		stopProcessing         int
		dedupNs, repeatNs      int64
		managedBy              string
		createdStr, updatedStr string
	)
	err := sc.Scan(&r.Name, &matchJSON, &provJSON, &isDefault, &disabled, &priority, &stopProcessing,
		&dedupNs, &repeatNs, &managedBy, &createdStr, &updatedStr)
	if err != nil {
		return models.Route{}, err
	}
	if matchJSON != "" && matchJSON != "{}" {
		_ = json.Unmarshal([]byte(matchJSON), &r.Match)
	}
	if provJSON != "" && provJSON != "[]" {
		_ = json.Unmarshal([]byte(provJSON), &r.Providers)
	}
	r.IsDefault = isDefault != 0
	r.Disabled = disabled != 0
	r.Priority = priority
	r.StopProcessing = stopProcessing != 0
	r.DedupWindow = models.Duration(dedupNs)
	r.RepeatInterval = models.Duration(repeatNs)
	r.ManagedBy = models.ManagedBy(managedBy)
	r.CreatedAt = parseTime(createdStr)
	r.UpdatedAt = parseTime(updatedStr)
	return r, nil
}

func orEmptyStrMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func orEmptySlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
