package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
)

// EnqueueDelivery inserts a new pending delivery due immediately and returns its id.
func (s *Store) EnqueueDelivery(ctx context.Context, d models.Delivery) (int64, error) {
	now := s.now()
	next := d.NextAttemptAt
	if next.IsZero() {
		next = now
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO deliveries (fingerprint, event_ref, route, provider, status, attempt_count,
		                        max_attempts, next_attempt_at, last_error, last_duration_ms,
		                        created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.Fingerprint, d.EventRef, d.Route, d.Provider, string(models.DeliveryPending), 0,
		d.MaxAttempts, fmtTime(next), "", 0, fmtTime(now), fmtTime(now))
	if err != nil {
		return 0, fmt.Errorf("enqueue delivery: %w", err)
	}
	return res.LastInsertId()
}

// ClaimDueDeliveries atomically transitions up to limit due deliveries
// (pending/failed with next_attempt_at <= now) to in_progress and returns them.
func (s *Store) ClaimDueDeliveries(ctx context.Context, limit int) ([]models.Delivery, error) {
	if limit <= 0 {
		limit = 1
	}
	nowStr := fmtTime(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin claim tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, selectDeliveryCols+`
		FROM deliveries
		WHERE status IN ('pending','failed') AND next_attempt_at <= ?
		ORDER BY next_attempt_at ASC, id ASC
		LIMIT ?`, nowStr, limit)
	if err != nil {
		return nil, fmt.Errorf("select due deliveries: %w", err)
	}
	var claimed []models.Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		claimed = append(claimed, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for i := range claimed {
		if _, err := tx.ExecContext(ctx,
			`UPDATE deliveries SET status='in_progress', updated_at=? WHERE id=?`,
			nowStr, claimed[i].ID); err != nil {
			return nil, fmt.Errorf("claim delivery %d: %w", claimed[i].ID, err)
		}
		claimed[i].Status = models.DeliveryInProgress
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim tx: %w", err)
	}
	return claimed, nil
}

// MarkDeliverySuccess records a successful delivery.
func (s *Store) MarkDeliverySuccess(ctx context.Context, id int64, attemptCount int, dur time.Duration) error {
	now := s.now()
	_, err := s.db.ExecContext(ctx, `
		UPDATE deliveries SET status=?, attempt_count=?, last_error='', last_duration_ms=?, updated_at=?
		WHERE id=?`,
		string(models.DeliverySuccess), attemptCount, dur.Milliseconds(), fmtTime(now), id)
	if err != nil {
		return fmt.Errorf("mark success: %w", err)
	}
	return nil
}

// MarkDeliveryRetry records a failed attempt that will be retried at nextAttemptAt.
func (s *Store) MarkDeliveryRetry(ctx context.Context, id int64, attemptCount int, nextAttemptAt time.Time, errMsg string, dur time.Duration) error {
	now := s.now()
	_, err := s.db.ExecContext(ctx, `
		UPDATE deliveries SET status=?, attempt_count=?, next_attempt_at=?, last_error=?, last_duration_ms=?, updated_at=?
		WHERE id=?`,
		string(models.DeliveryFailed), attemptCount, fmtTime(nextAttemptAt), truncErr(errMsg), dur.Milliseconds(), fmtTime(now), id)
	if err != nil {
		return fmt.Errorf("mark retry: %w", err)
	}
	return nil
}

// MarkDeliveryDead records a delivery that has exhausted all retries.
func (s *Store) MarkDeliveryDead(ctx context.Context, id int64, attemptCount int, errMsg string, dur time.Duration) error {
	now := s.now()
	_, err := s.db.ExecContext(ctx, `
		UPDATE deliveries SET status=?, attempt_count=?, last_error=?, last_duration_ms=?, updated_at=?
		WHERE id=?`,
		string(models.DeliveryDead), attemptCount, truncErr(errMsg), dur.Milliseconds(), fmtTime(now), id)
	if err != nil {
		return fmt.Errorf("mark dead: %w", err)
	}
	return nil
}

// ResetInProgress returns any deliveries stuck in_progress (e.g. from a crash)
// back to pending so they are retried. Returns the number reset.
func (s *Store) ResetInProgress(ctx context.Context) (int64, error) {
	now := s.now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE deliveries SET status='pending', next_attempt_at=?, updated_at=? WHERE status='in_progress'`,
		fmtTime(now), fmtTime(now))
	if err != nil {
		return 0, fmt.Errorf("reset in-progress: %w", err)
	}
	return res.RowsAffected()
}

// DeliveryFilter narrows ListDeliveries.
type DeliveryFilter struct {
	Fingerprint string
	Status      string
	Limit       int
}

// ListDeliveries returns deliveries newest-first.
func (s *Store) ListDeliveries(ctx context.Context, f DeliveryFilter) ([]models.Delivery, error) {
	var where []string
	var args []any
	if f.Fingerprint != "" {
		where = append(where, "fingerprint = ?")
		args = append(args, f.Fingerprint)
	}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	q := selectDeliveryCols + ` FROM deliveries`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id DESC"
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q += " LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list deliveries: %w", err)
	}
	defer rows.Close()
	var out []models.Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDelivery returns a delivery by id.
func (s *Store) GetDelivery(ctx context.Context, id int64) (models.Delivery, error) {
	row := s.db.QueryRowContext(ctx, selectDeliveryCols+` FROM deliveries WHERE id = ?`, id)
	d, err := scanDelivery(row)
	if errors.Is(err, sql.ErrNoRows) {
		return models.Delivery{}, ErrNotFound
	}
	return d, err
}

const selectDeliveryCols = `SELECT id, fingerprint, event_ref, route, provider, status,
	attempt_count, max_attempts, next_attempt_at, last_error, last_duration_ms, created_at, updated_at`

func scanDelivery(sc scanner) (models.Delivery, error) {
	var (
		d                  models.Delivery
		status             string
		nextStr            string
		durMs              int64
		createdStr, updStr string
	)
	err := sc.Scan(&d.ID, &d.Fingerprint, &d.EventRef, &d.Route, &d.Provider, &status,
		&d.AttemptCount, &d.MaxAttempts, &nextStr, &d.LastError, &durMs, &createdStr, &updStr)
	if err != nil {
		return models.Delivery{}, err
	}
	d.Status = models.DeliveryStatus(status)
	d.NextAttemptAt = parseTime(nextStr)
	d.LastDuration = models.Duration(time.Duration(durMs) * time.Millisecond)
	d.CreatedAt = parseTime(createdStr)
	d.UpdatedAt = parseTime(updStr)
	return d, nil
}

// truncErr bounds stored error strings to keep rows tidy.
func truncErr(s string) string {
	const max = 2000
	if len(s) > max {
		return s[:max]
	}
	return s
}
