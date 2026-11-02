package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/pod32g/omni-notify/internal/clock"
	"github.com/pod32g/omni-notify/internal/models"
)

func TestEventMessageRoundTrip(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	ev := sampleEvent()
	ev.Message = &models.Message{Title: "Down", Text: "Pi-hole is down", Channels: map[string]json.RawMessage{
		"telegram": json.RawMessage(`{"text":"<b>down</b>","parse_mode":"HTML"}`),
	}}
	stored, err := st.InsertEvent(ctx, ev, nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := st.InsertEvent(ctx, sampleEvent(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetEvent(ctx, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	var tg struct {
		Text      string `json:"text"`
		ParseMode string `json:"parse_mode"`
	}
	if got.Message == nil || got.Message.Title != "Down" || got.Message.Text != "Pi-hole is down" ||
		json.Unmarshal(got.Message.Channel("telegram"), &tg) != nil || tg.Text != "<b>down</b>" || tg.ParseMode != "HTML" {
		t.Fatalf("message %+v", got.Message)
	}
	if got, _ := st.GetEvent(ctx, plain.ID); got.Message != nil {
		t.Fatalf("an event without a message read back %+v", got.Message)
	}
}

func TestSentMessages(t *testing.T) {
	st, clk := newTestStore(t)
	ctx := context.Background()
	m := models.SentMessage{Channel: "telegram", Account: "123", Chat: "777", Ref: "pihole", MessageID: "41", Kind: "text"}
	if err := st.PutSentMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSentMessage(ctx, "telegram", "123", "777", "pihole")
	if err != nil || got.MessageID != "41" || got.Kind != "text" {
		t.Fatalf("got %+v %v", got, err)
	}
	if _, err := st.GetSentMessage(ctx, "telegram", "123", "778", "pihole"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another chat's ref: %v", err)
	}

	// The same ref later points at the newer message.
	m.MessageID, m.Kind = "52", "media"
	if err := st.PutSentMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetSentMessage(ctx, "telegram", "123", "777", "pihole"); got.MessageID != "52" || got.Kind != "media" {
		t.Fatalf("replaced %+v", got)
	}

	if err := st.DeleteSentMessage(ctx, "telegram", "123", "777", "pihole"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSentMessage(ctx, "telegram", "123", "777", "pihole"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted: %v", err)
	}

	// Past the retention a ref is gone, and pruned by the next write.
	if err := st.PutSentMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	clk.Advance(31 * 24 * time.Hour)
	if _, err := st.GetSentMessage(ctx, "telegram", "123", "777", "pihole"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired: %v", err)
	}
	m.Ref = "other"
	if err := st.PutSentMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM sent_messages`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows after prune: %d %v", n, err)
	}
}

// A database from before events had a message column is upgraded in place.
func TestMigrateAddsEventMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE events (
		id INTEGER PRIMARY KEY AUTOINCREMENT, event_id TEXT NOT NULL, type TEXT NOT NULL,
		source TEXT NOT NULL, status TEXT NOT NULL DEFAULT '', severity TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL, summary TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
		labels TEXT NOT NULL DEFAULT '{}', annotations TEXT NOT NULL DEFAULT '{}', timestamp TEXT NOT NULL,
		fingerprint TEXT NOT NULL, raw_payload TEXT NOT NULL DEFAULT '{}', received_at TEXT NOT NULL);
		INSERT INTO events (event_id, type, source, title, timestamp, fingerprint, received_at)
		VALUES ('old', 'alert', 'homelab', 'Old', '2026-06-28T19:00:00Z', 'fp-old', '2026-06-28T19:00:00Z');`)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(path, newTestCipher(t), clock.NewFake(time.Date(2026, 6, 28, 20, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	old, err := st.GetEvent(context.Background(), 1)
	if err != nil || old.Title != "Old" || old.Message != nil {
		t.Fatalf("old event %+v %v", old, err)
	}
}
