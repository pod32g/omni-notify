package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/pod32g/omni-notify/internal/clock"
	"github.com/pod32g/omni-notify/internal/models"
)

// A database made before calls knew their incident gains the column, and its
// calls read back without one.
func TestVoiceCallsGainIncidentColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE voice_calls (id TEXT PRIMARY KEY, event_ref INTEGER NOT NULL DEFAULT 0,
		agent TEXT NOT NULL, caller_name TEXT NOT NULL, simulated INTEGER NOT NULL DEFAULT 0, test INTEGER NOT NULL DEFAULT 0,
		recipient TEXT NOT NULL, type TEXT NOT NULL, severity TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '',
		message TEXT NOT NULL, details TEXT NOT NULL DEFAULT '', interactive INTEGER NOT NULL DEFAULT 1,
		user_requested INTEGER NOT NULL DEFAULT 0, idempotency_key TEXT NOT NULL DEFAULT '', state TEXT NOT NULL,
		outcome TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT '', attempt INTEGER NOT NULL DEFAULT 1,
		parent_call_id TEXT NOT NULL DEFAULT '', origin TEXT NOT NULL DEFAULT 'api', scheduled_at TEXT NOT NULL,
		expires_at TEXT NOT NULL, ring_deadline TEXT NOT NULL DEFAULT '', rang_at TEXT NOT NULL DEFAULT '',
		answered_at TEXT NOT NULL DEFAULT '', answered_by TEXT NOT NULL DEFAULT '', ended_at TEXT NOT NULL DEFAULT '',
		last_activity_at TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
		INSERT INTO voice_calls (id, agent, caller_name, recipient, type, message, state, scheduled_at, expires_at,
		last_activity_at, created_at, updated_at) VALUES ('old', 'claude', 'Claude', 'pod32g', 'notification', 'Hi.',
		'completed', '2026-10-26T22:45:00.000000000Z', '2026-10-27T22:45:00.000000000Z', '2026-10-26T22:45:00.000000000Z',
		'2026-10-26T22:45:00.000000000Z', '2026-10-26T22:45:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path, newTestCipher(t), clock.NewFake(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	old, err := st.GetCall(ctx, "old")
	if err != nil || old.Incident != nil {
		t.Fatalf("old call = %+v, %v", old, err)
	}
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	c := models.Call{ID: "new", Agent: "oncall", CallerName: "Omni On-Call", Recipient: "pod32g", Type: "page",
		Message: "Jellyfin is failing.", State: models.CallScheduled, ScheduledAt: now, ExpiresAt: now.Add(time.Hour),
		LastActivityAt: now, CreatedAt: now, UpdatedAt: now,
		Incident: &models.CallIncident{ID: "inc_128", Number: 128, Title: "Jellyfin", Severity: "sev1"}}
	if _, err := st.InsertCall(ctx, c, "test", ""); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetCall(ctx, "new")
	if err != nil || got.Incident == nil || *got.Incident != *c.Incident {
		t.Fatalf("new call = %+v, %v", got.Incident, err)
	}
}
