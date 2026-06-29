package notifier

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pod32g/omni-notify/internal/clock"
	"github.com/pod32g/omni-notify/internal/metrics"
	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/providers"
	"github.com/pod32g/omni-notify/internal/storage"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// stubRecorder is shared state for the stub provider across send attempts.
type stubRecorder struct {
	mu        sync.Mutex
	calls     int
	failUntil int // fail the first N calls
	messages  []models.NotificationMessage
}

func (r *stubRecorder) record(msg models.NotificationMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.messages = append(r.messages, msg)
	if r.calls <= r.failUntil {
		return fmt.Errorf("stub failure %d", r.calls)
	}
	return nil
}

func (r *stubRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type stubProvider struct{ rec *stubRecorder }

func (s *stubProvider) Send(_ context.Context, msg models.NotificationMessage) error {
	return s.rec.record(msg)
}

type harness struct {
	n     *Notifier
	store *storage.Store
	clk   *clock.Fake
	m     *metrics.Metrics
	rec   *stubRecorder
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 6, 28, 20, 0, 0, 0, time.UTC))
	store, err := storage.Open(filepath.Join(t.TempDir(), "n.db"), nil, clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	rec := &stubRecorder{}
	reg := providers.NewRegistry()
	reg.Register("stub", func(_ map[string]any, _ string) (providers.Provider, error) {
		return &stubProvider{rec: rec}, nil
	})
	m := metrics.New()
	n := New(store, reg, m, clk, nil, cfg)

	ctx := context.Background()
	if err := store.UpsertProvider(ctx, models.ProviderConfig{Name: "stub", Kind: "stub", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRoute(ctx, models.Route{Name: "all", Providers: []string{"stub"}}); err != nil {
		t.Fatal(err)
	}
	return &harness{n: n, store: store, clk: clk, m: m, rec: rec}
}

func firingEvent() models.Event {
	return models.Event{
		EventID: "e1", Type: "alert", Source: "homelab", Status: models.StatusFiring,
		Severity: models.SeverityCritical, Title: "Pi-hole Down",
		Timestamp: time.Date(2026, 6, 28, 19, 0, 0, 0, time.UTC), Fingerprint: "fp-1",
	}
}

func TestProcess_EnqueueDedupResolve(t *testing.T) {
	h := newHarness(t, Config{DefaultDedupWindow: 5 * time.Minute})
	ctx := context.Background()
	ev := firingEvent()

	res, err := h.n.Process(ctx, ev, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if res.DeliveriesEnqueued != 1 || res.Deduplicated {
		t.Fatalf("first firing: %+v", res)
	}

	// Repeat firing is suppressed (no repeat interval).
	res2, _ := h.n.Process(ctx, ev, []byte("{}"))
	if res2.DeliveriesEnqueued != 0 || !res2.Deduplicated {
		t.Fatalf("repeat firing should be deduplicated: %+v", res2)
	}

	// Resolve notifies once.
	ev.Status = models.StatusResolved
	res3, _ := h.n.Process(ctx, ev, []byte("{}"))
	if res3.DeliveriesEnqueued != 1 {
		t.Fatalf("resolve should enqueue: %+v", res3)
	}

	// State should now be inactive.
	st, err := h.store.GetState(ctx, "fp-1")
	if err != nil || st.Active {
		t.Fatalf("state should be inactive after resolve: %+v %v", st, err)
	}
}

func TestProcess_StatePreservesFirstSeen(t *testing.T) {
	h := newHarness(t, Config{})
	ctx := context.Background()
	ev := firingEvent()
	if _, err := h.n.Process(ctx, ev, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	first, _ := h.store.GetState(ctx, "fp-1")
	h.clk.Advance(time.Hour)
	if _, err := h.n.Process(ctx, ev, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	second, _ := h.store.GetState(ctx, "fp-1")
	if !second.FirstSeen.Equal(first.FirstSeen) {
		t.Fatalf("first_seen changed: %v -> %v", first.FirstSeen, second.FirstSeen)
	}
	if !second.LastSeen.After(first.LastSeen) {
		t.Fatalf("last_seen not advanced: %v -> %v", first.LastSeen, second.LastSeen)
	}
}

func TestDelivery_HappyPathViaEngine(t *testing.T) {
	h := newHarness(t, Config{Workers: 2, QueueSize: 16, MaxAttempts: 3,
		SendTimeout: 2 * time.Second, PollInterval: 5 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	if err := h.n.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); h.n.Stop() }()

	if _, err := h.n.Process(context.Background(), firingEvent(), []byte("{}")); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 3*time.Second, func() bool {
		ds, _ := h.store.ListDeliveries(context.Background(), storage.DeliveryFilter{})
		return len(ds) == 1 && ds[0].Status == models.DeliverySuccess
	})

	if h.rec.count() != 1 {
		t.Fatalf("expected 1 send, got %d", h.rec.count())
	}
	if got := testutil.ToFloat64(h.m.NotificationsSent.WithLabelValues("stub")); got != 1 {
		t.Fatalf("NotificationsSent = %v, want 1", got)
	}
	if h.rec.messages[0].Route != "all" {
		t.Fatalf("message route = %q", h.rec.messages[0].Route)
	}
}

func TestDelivery_RetryThenSucceed(t *testing.T) {
	h := newHarness(t, Config{MaxAttempts: 5, BackoffBase: time.Minute, BackoffFactor: 2})
	ctx := context.Background()
	h.rec.failUntil = 2 // first two attempts fail, third succeeds

	if _, err := h.n.Process(ctx, firingEvent(), []byte("{}")); err != nil {
		t.Fatal(err)
	}

	drive := func() {
		claimed, err := h.store.ClaimDueDeliveries(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range claimed {
			h.n.deliver(context.Background(), d)
		}
	}

	drive() // attempt 1 fails
	d := onlyDelivery(t, h)
	if d.Status != models.DeliveryFailed || d.AttemptCount != 1 {
		t.Fatalf("after attempt 1: %+v", d)
	}
	// Not yet due.
	if claimed, _ := h.store.ClaimDueDeliveries(ctx, 10); len(claimed) != 0 {
		t.Fatal("retry should not be due before backoff elapses")
	}

	h.clk.Advance(time.Minute)     // backoff(1) = 1m
	drive()                        // attempt 2 fails
	h.clk.Advance(3 * time.Minute) // backoff(2) = 2m (advance enough)
	drive()                        // attempt 3 succeeds

	d = onlyDelivery(t, h)
	if d.Status != models.DeliverySuccess || d.AttemptCount != 3 {
		t.Fatalf("expected success on attempt 3: %+v", d)
	}
	if h.rec.count() != 3 {
		t.Fatalf("expected 3 sends, got %d", h.rec.count())
	}
}

func TestDelivery_DeadAfterMaxAttempts(t *testing.T) {
	h := newHarness(t, Config{MaxAttempts: 2, BackoffBase: time.Minute, BackoffFactor: 2})
	ctx := context.Background()
	h.rec.failUntil = 100 // always fail

	if _, err := h.n.Process(ctx, firingEvent(), []byte("{}")); err != nil {
		t.Fatal(err)
	}
	drive := func() {
		claimed, _ := h.store.ClaimDueDeliveries(ctx, 10)
		for _, d := range claimed {
			h.n.deliver(context.Background(), d)
		}
	}
	drive()                    // attempt 1 -> failed
	h.clk.Advance(time.Minute) // due
	drive()                    // attempt 2 -> dead (max reached)

	d := onlyDelivery(t, h)
	if d.Status != models.DeliveryDead || d.AttemptCount != 2 {
		t.Fatalf("expected dead after 2 attempts: %+v", d)
	}
	if got := testutil.ToFloat64(h.m.NotificationsFailed.WithLabelValues("stub")); got != 1 {
		t.Fatalf("NotificationsFailed = %v, want 1", got)
	}
}

func TestDelivery_UnknownProviderMarkedDead(t *testing.T) {
	h := newHarness(t, Config{MaxAttempts: 3})
	ctx := context.Background()
	id, _ := h.store.EnqueueDelivery(ctx, models.Delivery{Fingerprint: "x", EventRef: 1,
		Route: "all", Provider: "ghost", MaxAttempts: 3})
	claimed, _ := h.store.ClaimDueDeliveries(ctx, 10)
	for _, d := range claimed {
		h.n.deliver(context.Background(), d)
	}
	d, _ := h.store.GetDelivery(ctx, id)
	if d.Status != models.DeliveryDead {
		t.Fatalf("unknown provider should be dead, got %q", d.Status)
	}
}

func TestProcess_NegativeDedupWindowDisables(t *testing.T) {
	h := newHarness(t, Config{DefaultDedupWindow: time.Hour})
	ctx := context.Background()
	mkInfo := func(fp string) models.Event {
		return models.Event{EventID: "i", Type: "log", Source: "s", Severity: models.SeverityInfo,
			Title: "t", Timestamp: time.Date(2026, 6, 28, 19, 0, 0, 0, time.UTC), Fingerprint: fp}
	}

	// Default route inherits the 1h window: the repeat is deduplicated.
	r1, _ := h.n.Process(ctx, mkInfo("fp-a"), []byte("{}"))
	r2, _ := h.n.Process(ctx, mkInfo("fp-a"), []byte("{}"))
	if r1.DeliveriesEnqueued != 1 || r2.DeliveriesEnqueued != 0 {
		t.Fatalf("default window should dedupe: r1=%+v r2=%+v", r1, r2)
	}

	// Negative window explicitly disables dedup, so both notify.
	if err := h.store.UpsertRoute(ctx, models.Route{Name: "all", Providers: []string{"stub"},
		DedupWindow: models.Duration(-1)}); err != nil {
		t.Fatal(err)
	}
	r3, _ := h.n.Process(ctx, mkInfo("fp-b"), []byte("{}"))
	r4, _ := h.n.Process(ctx, mkInfo("fp-b"), []byte("{}"))
	if r3.DeliveriesEnqueued != 1 || r4.DeliveriesEnqueued != 1 {
		t.Fatalf("negative window should disable dedup: r3=%+v r4=%+v", r3, r4)
	}
}

func TestProcess_ProviderDedupAcrossRoutes(t *testing.T) {
	h := newHarness(t, Config{})
	ctx := context.Background()
	// Two routes both target the same provider and both match/notify.
	if err := h.store.UpsertRoute(ctx, models.Route{Name: "all", Providers: []string{"stub"}, Priority: 10}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertRoute(ctx, models.Route{Name: "second", Providers: []string{"stub"}, Priority: 5}); err != nil {
		t.Fatal(err)
	}

	res, err := h.n.Process(ctx, firingEvent(), []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.RoutesMatched) != 2 {
		t.Fatalf("expected both routes matched, got %v", res.RoutesMatched)
	}
	if res.DeliveriesEnqueued != 1 {
		t.Fatalf("provider dedup failed: enqueued %d, want 1", res.DeliveriesEnqueued)
	}
	ds, _ := h.store.ListDeliveries(ctx, storage.DeliveryFilter{})
	if len(ds) != 1 {
		t.Fatalf("expected 1 delivery row, got %d", len(ds))
	}
	// Attributed to the highest-priority route.
	if ds[0].Route != "all" {
		t.Fatalf("delivery attributed to %q, want highest-priority route 'all'", ds[0].Route)
	}
}

// blockingProvider blocks in Send until its context is cancelled, so tests can
// observe an in-flight delivery during shutdown.
type blockingProvider struct{ started chan struct{} }

func (b blockingProvider) Send(ctx context.Context, _ models.NotificationMessage) error {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestEngineShutdownDrains(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 6, 28, 20, 0, 0, 0, time.UTC))
	store, err := storage.Open(filepath.Join(t.TempDir(), "s.db"), nil, clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	started := make(chan struct{}, 4)
	reg := providers.NewRegistry()
	reg.Register("block", func(map[string]any, string) (providers.Provider, error) {
		return blockingProvider{started: started}, nil
	})
	n := New(store, reg, metrics.New(), clk, nil, Config{Workers: 1, QueueSize: 8,
		MaxAttempts: 3, PollInterval: 5 * time.Millisecond, SendTimeout: 5 * time.Second})

	ctx := context.Background()
	if err := store.UpsertProvider(ctx, models.ProviderConfig{Name: "block", Kind: "block", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRoute(ctx, models.Route{Name: "all", Providers: []string{"block"}}); err != nil {
		t.Fatal(err)
	}

	engineCtx, cancel := context.WithCancel(context.Background())
	if err := n.Start(engineCtx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		ev := firingEvent()
		ev.Fingerprint = fmt.Sprintf("fp-%d", i)
		if _, err := n.Process(ctx, ev, []byte("{}")); err != nil {
			t.Fatal(err)
		}
	}

	// Wait until a send is in flight, then trigger shutdown.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery started")
	}
	cancel()

	done := make(chan struct{})
	go func() { n.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not return after context cancel")
	}

	// in_progress rows are recovered on the next boot via ResetInProgress; after
	// that no delivery should remain stuck in_progress.
	if _, err := store.ResetInProgress(ctx); err != nil {
		t.Fatal(err)
	}
	ds, _ := store.ListDeliveries(ctx, storage.DeliveryFilter{})
	for _, d := range ds {
		if d.Status == models.DeliveryInProgress {
			t.Fatalf("delivery %d left in_progress after reset", d.ID)
		}
	}
}

func onlyDelivery(t *testing.T, h *harness) models.Delivery {
	t.Helper()
	ds, err := h.store.ListDeliveries(context.Background(), storage.DeliveryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 {
		t.Fatalf("expected exactly 1 delivery, got %d", len(ds))
	}
	return ds[0]
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
