package oncall

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

// The plugin keeps its own SQLite file. Incidents, overrides and events are
// JSON documents with the columns queries need; the process holds the working
// copy in memory and writes every change through.
const schema = `
CREATE TABLE IF NOT EXISTS incidents (
	id     TEXT PRIMARY KEY,
	number INTEGER NOT NULL UNIQUE,
	data   TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
	seq         INTEGER PRIMARY KEY AUTOINCREMENT,
	at          TEXT NOT NULL,
	kind        TEXT NOT NULL,
	incident_id TEXT,
	schedule_id TEXT,
	data        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_incident ON events(incident_id, seq);
CREATE INDEX IF NOT EXISTS events_kind ON events(kind, seq);
CREATE TABLE IF NOT EXISTS alerts (
	service_id  TEXT NOT NULL,
	fingerprint TEXT NOT NULL,
	incident_id TEXT NOT NULL,
	PRIMARY KEY (service_id, fingerprint)
);
CREATE TABLE IF NOT EXISTS resolved_alerts (
	service_id  TEXT NOT NULL,
	fingerprint TEXT NOT NULL,
	at          TEXT NOT NULL,
	PRIMARY KEY (service_id, fingerprint)
);
CREATE TABLE IF NOT EXISTS overrides (
	id          TEXT PRIMARY KEY,
	schedule_id TEXT NOT NULL,
	data        TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS idempotency (
	user_id    TEXT NOT NULL,
	key        TEXT NOT NULL,
	status     INTEGER NOT NULL,
	body       BLOB NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY (user_id, key)
);
CREATE TABLE IF NOT EXISTS deliveries_seen (
	delivery_id INTEGER PRIMARY KEY,
	seen_at     TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

type store struct{ db *sql.DB }

func openStore(path string) (*store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)",
		url.PathEscape(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &store{db: db}, nil
}

func (s *store) close() error { return s.db.Close() }

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func (s *store) putIncident(ctx context.Context, inc *incident) error {
	data, err := json.Marshal(inc)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO incidents (id, number, data) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET data = excluded.data`, inc.ID, inc.Number, string(data))
	return err
}

func (s *store) incidents(ctx context.Context) ([]*incident, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM incidents ORDER BY number`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*incident
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		inc := &incident{}
		if err := json.Unmarshal([]byte(data), inc); err != nil {
			return nil, err
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

func (s *store) addEvent(ctx context.Context, e *event) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO events (at, kind, incident_id, schedule_id, data) VALUES (?, ?, ?, ?, ?)`,
		ts(e.At), e.Kind, nullable(e.IncidentID), nullable(e.ScheduleID), string(data))
	if err != nil {
		return err
	}
	e.Seq, err = res.LastInsertId()
	return err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// timeline is an incident's events, newest first.
func (s *store) timeline(ctx context.Context, incidentID string) ([]event, error) {
	return s.events(ctx, `SELECT seq, data FROM events WHERE incident_id = ? ORDER BY seq DESC`, incidentID)
}

// activity is the newest events of the given kinds.
func (s *store) activity(ctx context.Context, kinds []string, limit int) ([]event, error) {
	q := `SELECT seq, data FROM events WHERE kind IN (`
	args := make([]any, 0, len(kinds)+1)
	for i, k := range kinds {
		if i > 0 {
			q += ","
		}
		q += "?"
		args = append(args, k)
	}
	q += `) ORDER BY seq DESC LIMIT ?`
	return s.events(ctx, q, append(args, limit)...)
}

func (s *store) events(ctx context.Context, q string, args ...any) ([]event, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []event
	for rows.Next() {
		var seq int64
		var data string
		if err := rows.Scan(&seq, &data); err != nil {
			return nil, err
		}
		var e event
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return nil, err
		}
		e.Seq = seq
		out = append(out, e)
	}
	return out, rows.Err()
}

// alert finds the open incident for a firing alert.
func (s *store) alert(ctx context.Context, serviceID, fingerprint string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT incident_id FROM alerts WHERE service_id = ? AND fingerprint = ?`,
		serviceID, fingerprint).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (s *store) putAlert(ctx context.Context, serviceID, fingerprint, incidentID string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO alerts (service_id, fingerprint, incident_id) VALUES (?, ?, ?)
		ON CONFLICT(service_id, fingerprint) DO UPDATE SET incident_id = excluded.incident_id`, serviceID, fingerprint, incidentID)
	return err
}

// dropAlerts forgets an incident's alerts, so the next firing opens a new one.
func (s *store) dropAlerts(ctx context.Context, incidentID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM alerts WHERE incident_id = ?`, incidentID)
	return err
}

// resolvedAt is when an alert last resolved at its source, if within a day.
func (s *store) resolvedAt(ctx context.Context, serviceID, fingerprint string) (time.Time, bool, error) {
	var at string
	err := s.db.QueryRowContext(ctx, `SELECT at FROM resolved_alerts WHERE service_id = ? AND fingerprint = ?`,
		serviceID, fingerprint).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	return t, err == nil, err
}

func (s *store) putResolved(ctx context.Context, serviceID, fingerprint string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO resolved_alerts (service_id, fingerprint, at) VALUES (?, ?, ?)
		ON CONFLICT(service_id, fingerprint) DO UPDATE SET at = MAX(at, excluded.at)`, serviceID, fingerprint, ts(at))
	return err
}

func (s *store) putOverride(ctx context.Context, o override) error {
	data, err := json.Marshal(o)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO overrides (id, schedule_id, data) VALUES (?, ?, ?)`, o.ID, o.ScheduleID, string(data))
	return err
}

func (s *store) overrides(ctx context.Context) ([]override, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM overrides ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []override
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var o override
		if err := json.Unmarshal([]byte(data), &o); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// replay returns a stored response for a repeated idempotency key.
func (s *store) replay(ctx context.Context, userID, key string) (int, []byte, bool, error) {
	var status int
	var body []byte
	err := s.db.QueryRowContext(ctx, `SELECT status, body FROM idempotency WHERE user_id = ? AND key = ?`, userID, key).Scan(&status, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, false, nil
	}
	return status, body, err == nil, err
}

func (s *store) remember(ctx context.Context, userID, key string, status int, body []byte, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO idempotency (user_id, key, status, body, created_at) VALUES (?, ?, ?, ?, ?)`,
		userID, key, status, body, ts(now))
	return err
}

// hasDelivery reports whether a Notify delivery was already taken in, so a
// retried delivery doesn't count an alert twice.
func (s *store) hasDelivery(ctx context.Context, id int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries_seen WHERE delivery_id = ?`, id).Scan(&n)
	return n > 0, err
}

func (s *store) markDelivery(ctx context.Context, id int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO deliveries_seen (delivery_id, seen_at) VALUES (?, ?)`, id, ts(now))
	return err
}

// prune drops idempotency keys, resolve times and delivery ids older than a day.
func (s *store) prune(ctx context.Context, now time.Time) error {
	cut := ts(now.Add(-24 * time.Hour))
	if _, err := s.db.ExecContext(ctx, `DELETE FROM idempotency WHERE created_at < ?`, cut); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM resolved_alerts WHERE at < ?`, cut); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM deliveries_seen WHERE seen_at < ?`, cut)
	return err
}

func (s *store) meta(ctx context.Context, key string, v any) error {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), v)
}

func (s *store) setMeta(ctx context.Context, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, string(raw))
	return err
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
