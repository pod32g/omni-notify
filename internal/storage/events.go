package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pod32g/omni-notify/internal/models"
)

// EventFilter narrows ListEvents. Zero-value fields are ignored.
type EventFilter struct {
	Source      string
	Type        string
	Severity    string
	Status      string
	Fingerprint string
	Limit       int
	Offset      int
}

// InsertEvent stores an event with the supplied raw payload and returns it with
// its server-assigned id and received_at timestamp.
func (s *Store) InsertEvent(ctx context.Context, ev models.Event, raw []byte) (models.StoredEvent, error) {
	received := s.now()
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO events (event_id, type, source, status, severity, title, summary,
		                    description, labels, annotations, timestamp, fingerprint,
		                    raw_payload, received_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		ev.EventID, ev.Type, ev.Source, string(ev.Status), string(ev.Severity), ev.Title,
		ev.Summary, ev.Description, mustJSONMap(ev.Labels), mustJSONMap(ev.Annotations),
		fmtTime(ev.Timestamp), ev.Fingerprint, string(raw), fmtTime(received),
	)
	if err != nil {
		return models.StoredEvent{}, fmt.Errorf("insert event: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return models.StoredEvent{}, fmt.Errorf("event id: %w", err)
	}
	return models.StoredEvent{Event: ev, ID: id, ReceivedAt: received}, nil
}

// GetEvent fetches a single stored event by id.
func (s *Store) GetEvent(ctx context.Context, id int64) (models.StoredEvent, error) {
	row := s.db.QueryRowContext(ctx, selectEventCols+` FROM events WHERE id = ?`, id)
	ev, err := scanEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return models.StoredEvent{}, ErrNotFound
	}
	return ev, err
}

// ListEvents returns stored events newest-first, filtered and paginated.
func (s *Store) ListEvents(ctx context.Context, f EventFilter) ([]models.StoredEvent, error) {
	var where []string
	var args []any
	add := func(col, val string) {
		if val != "" {
			where = append(where, col+" = ?")
			args = append(args, val)
		}
	}
	add("source", f.Source)
	add("type", f.Type)
	add("severity", f.Severity)
	add("status", f.Status)
	add("fingerprint", f.Fingerprint)

	q := selectEventCols + ` FROM events`
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
	if f.Offset > 0 {
		q += " OFFSET ?"
		args = append(args, f.Offset)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	var out []models.StoredEvent
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

const selectEventCols = `SELECT id, event_id, type, source, status, severity, title,
	summary, description, labels, annotations, timestamp, fingerprint, received_at`

// scanner abstracts *sql.Row and *sql.Rows for shared scanning.
type scanner interface {
	Scan(dest ...any) error
}

func scanEvent(sc scanner) (models.StoredEvent, error) {
	var (
		ev                          models.StoredEvent
		status, severity            string
		labelsJSON, annotationsJSON string
		tsStr, recvStr              string
	)
	err := sc.Scan(&ev.ID, &ev.EventID, &ev.Type, &ev.Source, &status, &severity, &ev.Title,
		&ev.Summary, &ev.Description, &labelsJSON, &annotationsJSON, &tsStr, &ev.Fingerprint, &recvStr)
	if err != nil {
		return models.StoredEvent{}, err
	}
	ev.Status = models.Status(status)
	ev.Severity = models.Severity(severity)
	ev.Labels = parseJSONMap(labelsJSON)
	ev.Annotations = parseJSONMap(annotationsJSON)
	ev.Timestamp = parseTime(tsStr)
	ev.ReceivedAt = parseTime(recvStr)
	return ev, nil
}

func mustJSONMap(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func parseJSONMap(s string) map[string]string {
	if s == "" || s == "{}" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}
