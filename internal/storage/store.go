// Package storage persists Omni-Notify's events, states, providers, routes and
// deliveries in SQLite, and encrypts provider secrets at rest.
package storage

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/pod32g/omni-notify/internal/clock"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// Store is the SQLite-backed persistence layer. All time values are stored as
// RFC3339Nano UTC strings; durations as integer nanoseconds.
type Store struct {
	db     *sql.DB
	cipher *Cipher
	clock  clock.Clock
}

// timeFormat is the canonical on-disk timestamp format. It uses a fixed-width
// 9-digit fractional second so that lexicographic string comparison matches
// chronological order — the delivery queue relies on `next_attempt_at <= now`
// and `ORDER BY` comparisons being done on these TEXT columns.
const timeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// Open opens (creating if needed) the SQLite database at path, applies the
// schema, and returns a ready Store. A nil cipher disables secret storage.
func Open(path string, cipher *Cipher, clk clock.Clock) (*Store, error) {
	if clk == nil {
		clk = clock.Real{}
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_pragma=synchronous(NORMAL)",
		url.PathEscape(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite tolerates a single writer; serialise access to avoid SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	s := &Store{db: db, cipher: cipher, clock: clk}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// migrate applies idempotent schema upgrades for columns added after the initial
// release, so that databases created by older versions keep working.
func (s *Store) migrate() error {
	adds := []struct{ table, col, ddl string }{
		{"routes", "priority", "ALTER TABLE routes ADD COLUMN priority INTEGER NOT NULL DEFAULT 0"},
		{"routes", "stop_processing", "ALTER TABLE routes ADD COLUMN stop_processing INTEGER NOT NULL DEFAULT 0"},
	}
	for _, a := range adds {
		has, err := s.columnExists(a.table, a.col)
		if err != nil {
			return err
		}
		if !has {
			if _, err := s.db.Exec(a.ddl); err != nil {
				return fmt.Errorf("add %s.%s: %w", a.table, a.col, err)
			}
		}
	}
	return nil
}

// columnExists reports whether table has a column named col.
func (s *Store) columnExists(table, col string) (bool, error) {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Ping verifies connectivity (used by /healthz).
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// now returns the store clock's current UTC time.
func (s *Store) now() time.Time { return s.clock.Now().UTC() }

func fmtTime(t time.Time) string { return t.UTC().Format(timeFormat) }

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	// Try the canonical fixed-width format first, then fall back to the more
	// lenient RFC3339 variants for any values written by earlier versions.
	for _, layout := range []string{timeFormat, time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
