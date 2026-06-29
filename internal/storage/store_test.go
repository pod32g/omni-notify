package storage

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pod32g/omni-notify/internal/clock"
	"github.com/pod32g/omni-notify/internal/models"
)

func newTestStore(t *testing.T) (*Store, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 6, 28, 20, 0, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(path, newTestCipher(t), clk)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, clk
}

func sampleEvent() models.Event {
	return models.Event{
		EventID:     "pihole-down",
		Type:        "alert",
		Source:      "homelab",
		Status:      models.StatusFiring,
		Severity:    models.SeverityCritical,
		Title:       "Pi-hole Down",
		Summary:     "not responding",
		Labels:      map[string]string{"service": "pihole"},
		Annotations: map[string]string{"runbook": "restart"},
		Timestamp:   time.Date(2026, 6, 28, 19, 0, 0, 0, time.UTC),
		Fingerprint: "fp-1",
	}
}

func TestEventRoundTrip(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	stored, err := st.InsertEvent(ctx, sampleEvent(), []byte(`{"raw":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID == 0 || stored.ReceivedAt.IsZero() {
		t.Fatalf("expected id and received_at, got %+v", stored)
	}
	got, err := st.GetEvent(ctx, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Pi-hole Down" || got.Labels["service"] != "pihole" || got.Severity != models.SeverityCritical {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// filters
	list, err := st.ListEvents(ctx, EventFilter{Source: "homelab"})
	if err != nil || len(list) != 1 {
		t.Fatalf("expected 1 by source, got %d (%v)", len(list), err)
	}
	none, _ := st.ListEvents(ctx, EventFilter{Source: "nope"})
	if len(none) != 0 {
		t.Fatalf("expected 0 by bad source, got %d", len(none))
	}
}

func TestGetEventNotFound(t *testing.T) {
	st, _ := newTestStore(t)
	if _, err := st.GetEvent(context.Background(), 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestStateAndActiveCount(t *testing.T) {
	st, clk := newTestStore(t)
	ctx := context.Background()
	now := clk.Now()
	s := models.State{Fingerprint: "fp-1", EventID: "e", Type: "alert", Source: "h",
		Status: models.StatusFiring, Title: "t", Active: true, FirstSeen: now, LastSeen: now}
	if err := st.UpsertState(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetState(ctx, "fp-1")
	if err != nil || !got.Active {
		t.Fatalf("expected active state, got %+v %v", got, err)
	}
	n, _ := st.CountActiveStates(ctx)
	if n != 1 {
		t.Fatalf("active count = %d, want 1", n)
	}
	// resolve
	s.Active = false
	s.Status = models.StatusResolved
	if err := st.UpsertState(ctx, s); err != nil {
		t.Fatal(err)
	}
	n, _ = st.CountActiveStates(ctx)
	if n != 0 {
		t.Fatalf("active count after resolve = %d, want 0", n)
	}
}

func TestRouteDedupRoundTrip(t *testing.T) {
	st, clk := newTestStore(t)
	ctx := context.Background()
	_, exists, err := st.GetRouteDedup(ctx, "fp", "r")
	if err != nil || exists {
		t.Fatalf("expected not-exists, got exists=%v err=%v", exists, err)
	}
	rd := models.RouteDedup{Fingerprint: "fp", Route: "r", Active: true,
		LastStatus: models.StatusFiring, RepeatCount: 2, LastNotifiedAt: clk.Now()}
	if err := st.UpsertRouteDedup(ctx, rd); err != nil {
		t.Fatal(err)
	}
	got, exists, err := st.GetRouteDedup(ctx, "fp", "r")
	if err != nil || !exists {
		t.Fatalf("expected exists, got %v %v", exists, err)
	}
	if !got.Active || got.RepeatCount != 2 || got.LastStatus != models.StatusFiring {
		t.Fatalf("dedup round-trip mismatch: %+v", got)
	}
}

func TestProviderSecretEncryptionAndPreservation(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	p := models.ProviderConfig{Name: "d", Kind: "discord", Secret: "https://hook", Enabled: true,
		Config: map[string]any{"username": "bot"}}
	if err := st.UpsertProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetProvider(ctx, "d")
	if err != nil {
		t.Fatal(err)
	}
	if got.Secret != "https://hook" || got.Config["username"] != "bot" {
		t.Fatalf("provider round-trip mismatch: %+v", got)
	}
	if has, _ := st.HasEncryptedSecrets(ctx); !has {
		t.Fatal("expected HasEncryptedSecrets true")
	}

	// Update without secret preserves existing secret.
	if err := st.UpsertProvider(ctx, models.ProviderConfig{Name: "d", Kind: "discord", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	got2, _ := st.GetProvider(ctx, "d")
	if got2.Secret != "https://hook" {
		t.Fatalf("secret not preserved on update: %q", got2.Secret)
	}
	if got2.Enabled {
		t.Fatal("enabled flag not updated")
	}
}

// TestProviderSecretEncryptedAtRest inspects the raw secret column to prove the
// plaintext is never stored unencrypted.
func TestProviderSecretEncryptedAtRest(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	const plain = "https://hooks.example.com/super-secret-token"
	if err := st.UpsertProvider(ctx, models.ProviderConfig{Name: "d", Kind: "discord", Secret: plain, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := st.db.QueryRowContext(ctx, `SELECT secret FROM providers WHERE name=?`, "d").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("secret column is empty")
	}
	if bytes.Contains(raw, []byte("super-secret-token")) || bytes.Contains(raw, []byte(plain)) {
		t.Fatalf("plaintext secret found at rest: %q", raw)
	}
	got, err := st.cipher.Decrypt(raw)
	if err != nil || got != plain {
		t.Fatalf("decrypt mismatch: %q %v", got, err)
	}
}

// TestTimeFormatLexicalOrder ensures the on-disk timestamp format sorts
// lexically in chronological order, including across the whole-second boundary
// that broke the delivery queue's due-time comparison.
func TestTimeFormatLexicalOrder(t *testing.T) {
	whole := time.Date(2026, 6, 28, 20, 0, 57, 0, time.UTC)
	frac := whole.Add(500 * time.Millisecond)
	if !(fmtTime(whole) < fmtTime(frac)) {
		t.Fatalf("lexical order broken: %q !< %q", fmtTime(whole), fmtTime(frac))
	}
}

// TestDeliveryDueAcrossSecondBoundary verifies a delivery scheduled on a whole
// second becomes due once the clock advances by a fraction of a second.
func TestDeliveryDueAcrossSecondBoundary(t *testing.T) {
	st, clk := newTestStore(t)
	ctx := context.Background()
	clk.Set(time.Date(2026, 6, 28, 20, 0, 57, 0, time.UTC)) // whole second
	id, err := st.EnqueueDelivery(ctx, models.Delivery{Fingerprint: "fp", Route: "r", Provider: "p", MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(500 * time.Millisecond) // fractional now > whole-second next_attempt_at
	claimed, err := st.ClaimDueDeliveries(ctx, 10)
	if err != nil || len(claimed) != 1 || claimed[0].ID != id {
		t.Fatalf("expected delivery due across boundary, got %v (%v)", claimed, err)
	}
}

func TestRouteRoundTrip(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	r := models.Route{Name: "crit", Match: map[string]string{"severity": "critical"},
		Providers: []string{"d"}, RepeatInterval: models.Duration(time.Hour), DedupWindow: models.Duration(5 * time.Minute)}
	if err := st.UpsertRoute(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetRoute(ctx, "crit")
	if err != nil {
		t.Fatal(err)
	}
	if got.Match["severity"] != "critical" || got.Providers[0] != "d" ||
		got.RepeatInterval.D() != time.Hour || got.DedupWindow.D() != 5*time.Minute {
		t.Fatalf("route round-trip mismatch: %+v", got)
	}
	if got.ManagedBy != models.ManagedByAPI {
		t.Fatalf("default managed_by should be api, got %q", got.ManagedBy)
	}
}

func TestDeliveryQueueLifecycle(t *testing.T) {
	st, clk := newTestStore(t)
	ctx := context.Background()
	id, err := st.EnqueueDelivery(ctx, models.Delivery{Fingerprint: "fp", EventRef: 1,
		Route: "r", Provider: "p", MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}

	// Claim it.
	claimed, err := st.ClaimDueDeliveries(ctx, 10)
	if err != nil || len(claimed) != 1 || claimed[0].ID != id {
		t.Fatalf("claim failed: %v %v", claimed, err)
	}
	if claimed[0].Status != models.DeliveryInProgress {
		t.Fatalf("claimed status = %q", claimed[0].Status)
	}
	// Second claim returns nothing (already in_progress).
	again, _ := st.ClaimDueDeliveries(ctx, 10)
	if len(again) != 0 {
		t.Fatalf("expected no re-claim, got %d", len(again))
	}

	// Mark retry in the future; not due yet.
	next := clk.Now().Add(time.Minute)
	if err := st.MarkDeliveryRetry(ctx, id, 1, next, "boom", 5*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	due, _ := st.ClaimDueDeliveries(ctx, 10)
	if len(due) != 0 {
		t.Fatalf("retry should not be due yet, got %d", len(due))
	}
	// Advance clock; now due.
	clk.Advance(2 * time.Minute)
	due, _ = st.ClaimDueDeliveries(ctx, 10)
	if len(due) != 1 || due[0].AttemptCount != 1 {
		t.Fatalf("expected 1 due retry with attempt=1, got %+v", due)
	}

	// Success.
	if err := st.MarkDeliverySuccess(ctx, id, 2, 7*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetDelivery(ctx, id)
	if got.Status != models.DeliverySuccess || got.AttemptCount != 2 {
		t.Fatalf("expected success/attempt 2, got %+v", got)
	}
}

func TestSuccessfulDeliveryNotReclaimed(t *testing.T) {
	st, clk := newTestStore(t)
	ctx := context.Background()
	id, _ := st.EnqueueDelivery(ctx, models.Delivery{Fingerprint: "fp", Route: "r", Provider: "p", MaxAttempts: 3})
	claimed, _ := st.ClaimDueDeliveries(ctx, 10)
	if len(claimed) != 1 {
		t.Fatalf("expected 1 claimed, got %d", len(claimed))
	}
	if err := st.MarkDeliverySuccess(ctx, id, 1, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour) // even much later
	if again, _ := st.ClaimDueDeliveries(ctx, 10); len(again) != 0 {
		t.Fatalf("successful delivery was re-claimed: %v", again)
	}
}

func TestConcurrentClaimNoDuplicate(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	const n = 20
	for i := 0; i < n; i++ {
		if _, err := st.EnqueueDelivery(ctx, models.Delivery{Fingerprint: "fp", Route: "r", Provider: "p", MaxAttempts: 3}); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	for w := 0; w < 5; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				claimed, err := st.ClaimDueDeliveries(ctx, 3)
				if err != nil || len(claimed) == 0 {
					return
				}
				mu.Lock()
				for _, d := range claimed {
					seen[d.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != n {
		t.Fatalf("claimed %d distinct rows, want %d", len(seen), n)
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("delivery %d claimed %d times (must be exactly once)", id, count)
		}
	}
}

func TestResetInProgress(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	id, _ := st.EnqueueDelivery(ctx, models.Delivery{Fingerprint: "fp", Route: "r", Provider: "p", MaxAttempts: 3})
	if _, err := st.ClaimDueDeliveries(ctx, 10); err != nil {
		t.Fatal(err)
	}
	n, err := st.ResetInProgress(ctx)
	if err != nil || n != 1 {
		t.Fatalf("reset = %d (%v), want 1", n, err)
	}
	got, _ := st.GetDelivery(ctx, id)
	if got.Status != models.DeliveryPending {
		t.Fatalf("expected pending after reset, got %q", got.Status)
	}
}
