package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/pod32g/omni-notify/internal/models"
)

// UpsertState inserts or updates the per-fingerprint event state. first_seen is
// preserved across updates; last_seen and the latest event details are refreshed.
func (s *Store) UpsertState(ctx context.Context, st models.State) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO states (fingerprint, event_id, type, source, status, severity, title,
		                    labels, active, first_seen, last_seen)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(fingerprint) DO UPDATE SET
			event_id = excluded.event_id,
			type     = excluded.type,
			source   = excluded.source,
			status   = excluded.status,
			severity = excluded.severity,
			title    = excluded.title,
			labels   = excluded.labels,
			active   = excluded.active,
			last_seen = excluded.last_seen`,
		st.Fingerprint, st.EventID, st.Type, st.Source, string(st.Status), string(st.Severity),
		st.Title, mustJSONMap(st.Labels), boolToInt(st.Active), fmtTime(st.FirstSeen), fmtTime(st.LastSeen),
	)
	if err != nil {
		return fmt.Errorf("upsert state: %w", err)
	}
	return nil
}

// GetState returns the current state for a fingerprint.
func (s *Store) GetState(ctx context.Context, fingerprint string) (models.State, error) {
	row := s.db.QueryRowContext(ctx, selectStateCols+` FROM states WHERE fingerprint = ?`, fingerprint)
	st, err := scanState(row)
	if errors.Is(err, sql.ErrNoRows) {
		return models.State{}, ErrNotFound
	}
	return st, err
}

// ListStates returns states, optionally only those currently active.
func (s *Store) ListStates(ctx context.Context, activeOnly bool) ([]models.State, error) {
	q := selectStateCols + ` FROM states`
	if activeOnly {
		q += ` WHERE active = 1`
	}
	q += ` ORDER BY last_seen DESC LIMIT 1000`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list states: %w", err)
	}
	defer rows.Close()
	var out []models.State
	for rows.Next() {
		st, err := scanState(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// CountActiveStates returns the number of currently active states.
func (s *Store) CountActiveStates(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM states WHERE active = 1`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count active states: %w", err)
	}
	return n, nil
}

const selectStateCols = `SELECT fingerprint, event_id, type, source, status, severity,
	title, labels, active, first_seen, last_seen`

func scanState(sc scanner) (models.State, error) {
	var (
		st                models.State
		status, severity  string
		labelsJSON        string
		active            int
		firstStr, lastStr string
	)
	err := sc.Scan(&st.Fingerprint, &st.EventID, &st.Type, &st.Source, &status, &severity,
		&st.Title, &labelsJSON, &active, &firstStr, &lastStr)
	if err != nil {
		return models.State{}, err
	}
	st.Status = models.Status(status)
	st.Severity = models.Severity(severity)
	st.Labels = parseJSONMap(labelsJSON)
	st.Active = active != 0
	st.FirstSeen = parseTime(firstStr)
	st.LastSeen = parseTime(lastStr)
	return st, nil
}

// GetRouteDedup returns the per-(fingerprint, route) dedup record. If none
// exists it returns a zero-valued record and ok=false.
func (s *Store) GetRouteDedup(ctx context.Context, fingerprint, route string) (models.RouteDedup, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT fingerprint, route, last_status, active, repeat_count, last_notified_at
		FROM route_dedup WHERE fingerprint = ? AND route = ?`, fingerprint, route)
	var (
		rd         models.RouteDedup
		status     string
		active     int
		notifiedAt string
	)
	err := row.Scan(&rd.Fingerprint, &rd.Route, &status, &active, &rd.RepeatCount, &notifiedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return models.RouteDedup{Fingerprint: fingerprint, Route: route}, false, nil
	}
	if err != nil {
		return models.RouteDedup{}, false, fmt.Errorf("get route dedup: %w", err)
	}
	rd.LastStatus = models.Status(status)
	rd.Active = active != 0
	rd.LastNotifiedAt = parseTime(notifiedAt)
	return rd, true, nil
}

// UpsertRouteDedup writes the per-(fingerprint, route) dedup record.
func (s *Store) UpsertRouteDedup(ctx context.Context, rd models.RouteDedup) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO route_dedup (fingerprint, route, last_status, active, repeat_count, last_notified_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(fingerprint, route) DO UPDATE SET
			last_status      = excluded.last_status,
			active           = excluded.active,
			repeat_count     = excluded.repeat_count,
			last_notified_at = excluded.last_notified_at`,
		rd.Fingerprint, rd.Route, string(rd.LastStatus), boolToInt(rd.Active),
		rd.RepeatCount, fmtTime(rd.LastNotifiedAt),
	)
	if err != nil {
		return fmt.Errorf("upsert route dedup: %w", err)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
