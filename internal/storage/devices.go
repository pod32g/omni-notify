package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
)

const selectDeviceCols = `SELECT id, user, name, platform, bundle_id, apns_environment, voip_token,
	secret_hash, disabled, created_at, last_seen_at`

// CreateEnrollment stores a one-time enrollment code (by hash) for a user.
func (s *Store) CreateEnrollment(ctx context.Context, codeHash, user string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO voice_enrollments (code_hash, user, created_at, expires_at)
		VALUES (?,?,?,?)`, codeHash, user, fmtTime(s.now()), fmtTime(expires))
	if err != nil {
		return fmt.Errorf("create enrollment: %w", err)
	}
	return nil
}

// RedeemEnrollment consumes an unexpired, unused code and creates the device
// for the code's user, atomically. ErrNotFound means the code is unknown,
// used or expired (deliberately indistinguishable to the caller).
func (s *Store) RedeemEnrollment(ctx context.Context, codeHash string, d models.Device) (models.Device, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return models.Device{}, fmt.Errorf("begin enrollment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := s.now()
	var user string
	err = tx.QueryRowContext(ctx, `SELECT user FROM voice_enrollments
		WHERE code_hash = ? AND used_at = '' AND expires_at > ?`, codeHash, fmtTime(now)).Scan(&user)
	if errors.Is(err, sql.ErrNoRows) {
		return models.Device{}, ErrNotFound
	}
	if err != nil {
		return models.Device{}, err
	}
	d.User = user
	d.CreatedAt = now
	if _, err := tx.ExecContext(ctx, `INSERT INTO voice_devices (id, user, name, platform, bundle_id,
		apns_environment, voip_token, secret_hash, disabled, created_at, last_seen_at)
		VALUES (?,?,?,?,?,?,?,?,0,?,?)`, d.ID, d.User, d.Name, d.Platform, d.BundleID, d.APNSEnvironment,
		d.VoIPToken, d.SecretHash, fmtTime(now), fmtTime(now)); err != nil {
		return models.Device{}, fmt.Errorf("insert device: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE voice_enrollments SET used_at = ?, device_id = ? WHERE code_hash = ?`,
		fmtTime(now), d.ID, codeHash); err != nil {
		return models.Device{}, fmt.Errorf("consume enrollment: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return models.Device{}, fmt.Errorf("commit enrollment: %w", err)
	}
	d.HasVoIPToken = d.VoIPToken != ""
	d.LastSeenAt = &now
	return d, nil
}

// DeviceBySecretHash finds an enabled device by its credential hash.
func (s *Store) DeviceBySecretHash(ctx context.Context, hash string) (models.Device, error) {
	d, err := scanDevice(s.db.QueryRowContext(ctx, selectDeviceCols+
		` FROM voice_devices WHERE secret_hash = ? AND disabled = 0`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return models.Device{}, ErrNotFound
	}
	return d, err
}

// GetDevice returns a device by id.
func (s *Store) GetDevice(ctx context.Context, id string) (models.Device, error) {
	d, err := scanDevice(s.db.QueryRowContext(ctx, selectDeviceCols+` FROM voice_devices WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return models.Device{}, ErrNotFound
	}
	return d, err
}

// ListDevices returns devices, optionally only one user's enabled ones.
func (s *Store) ListDevices(ctx context.Context, user string, enabledOnly bool) ([]models.Device, error) {
	q := selectDeviceCols + ` FROM voice_devices WHERE 1=1`
	var args []any
	if user != "" {
		q += ` AND user = ?`
		args = append(args, user)
	}
	if enabledOnly {
		q += ` AND disabled = 0`
	}
	q += ` ORDER BY created_at ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	defer rows.Close()
	var out []models.Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpdateDevicePush replaces a device's VoIP token and APNs environment (the
// token changes on reinstall and sometimes on OS updates) and marks it seen.
func (s *Store) UpdateDevicePush(ctx context.Context, id, token, env string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE voice_devices SET voip_token = ?, apns_environment = ?, last_seen_at = ?
		WHERE id = ?`, token, env, fmtTime(s.now()), id)
	if err != nil {
		return fmt.Errorf("update device push: %w", err)
	}
	return nil
}

// TouchDevice marks a device as seen now.
func (s *Store) TouchDevice(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE voice_devices SET last_seen_at = ? WHERE id = ?`, fmtTime(s.now()), id)
	return err
}

// SetDeviceDisabled enables or disables a device. Disabled devices are never
// rung and their credential stops working.
func (s *Store) SetDeviceDisabled(ctx context.Context, id string, disabled bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE voice_devices SET disabled = ? WHERE id = ?`, boolInt(disabled), id)
	if err != nil {
		return fmt.Errorf("set device disabled: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanDevice(sc scanner) (models.Device, error) {
	var (
		d                 models.Device
		disabled          int
		created, lastSeen string
	)
	if err := sc.Scan(&d.ID, &d.User, &d.Name, &d.Platform, &d.BundleID, &d.APNSEnvironment, &d.VoIPToken,
		&d.SecretHash, &disabled, &created, &lastSeen); err != nil {
		return models.Device{}, err
	}
	d.Disabled = disabled != 0
	d.HasVoIPToken = d.VoIPToken != ""
	d.CreatedAt = parseTime(created)
	d.LastSeenAt = parseOptTime(lastSeen)
	return d, nil
}
