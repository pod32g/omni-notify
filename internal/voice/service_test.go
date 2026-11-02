package voice

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pod32g/omni-notify/internal/clock"
	"github.com/pod32g/omni-notify/internal/config"
	"github.com/pod32g/omni-notify/internal/metrics"
	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/storage"
)

const testConfig = `
security:
  tokens: ["admin"]
voice:
  enabled: true
  ring_timeout: 45s
  device_ring_timeout: 30s
  agents:
    - id: claude
      display_name: Claude
      allow_test: true
      may_override_quiet_hours_for_user_reminders: true
    - id: gpt
      display_name: GPT
    - id: omni-oncall
      display_name: Omni Oncall
      may_override_quiet_hours_when_critical: true
      actions: [acknowledge]
    - id: stranger
  users:
    - id: pod32g
      aliases: [me]
      timezone: America/Mexico_City
      allowed_agents: [claude, gpt, omni-oncall]
      quiet_hours: {start: "23:00", end: "08:00"}
      exceptions: {explicit_user_reminders: true, critical_incidents: true}
      rate_limit: {max_calls_per_hour: 3, cooldown: 1m}
      retry: {max_attempts: 2, delay: 2m}
      max_snooze: 12h
      fallback_provider: discord-home
`

type fakePusher struct {
	mu   sync.Mutex
	sent []Invitation
	err  error
}

func (p *fakePusher) Push(_ context.Context, _ models.Device, inv Invitation) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.sent = append(p.sent, inv)
	return nil
}

func (p *fakePusher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sent)
}

type fakeFallback struct{ sent []int64 }

func (f *fakeFallback) SendFallback(_ context.Context, ref int64, _ string) error {
	f.sent = append(f.sent, ref)
	return nil
}

type harness struct {
	t        *testing.T
	svc      *Service
	store    *storage.Store
	clk      *clock.Fake
	push     *fakePusher
	fallback *fakeFallback
	dbPath   string
	cfg      config.Config
}

// 4:45 PM Monday in Mexico City: outside quiet hours.
var afternoon = time.Date(2026, 10, 26, 22, 45, 0, 0, time.UTC)

func newHarness(t *testing.T, start time.Time) *harness {
	t.Helper()
	cfg, err := config.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, clk: clock.NewFake(start), push: &fakePusher{}, fallback: &fakeFallback{},
		dbPath: filepath.Join(t.TempDir(), "voice.db"), cfg: cfg}
	h.open()
	return h
}

func (h *harness) open() {
	store, err := storage.Open(h.dbPath, nil, h.clk)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { store.Close() })
	h.store = store
	h.svc = NewService(h.cfg.Voice, store, metrics.New(), Options{Pusher: h.push, Fallback: h.fallback, Clock: h.clk})
}

// enroll adds a phone for pod32g and returns it.
func (h *harness) enroll(name string) models.Device {
	h.t.Helper()
	code, _, err := h.svc.CreateEnrollment(context.Background(), "me")
	if err != nil {
		h.t.Fatal(err)
	}
	d, secret, err := h.svc.Enroll(context.Background(), EnrollRequest{Code: strings.ToLower(code), Name: name,
		BundleID: "dev.pod32g.OmniNotify", VoIPToken: "abcdef0123", APNSEnvironment: "development"})
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.svc.AuthenticateDevice(context.Background(), secret); err != nil {
		h.t.Fatal(err)
	}
	return d
}

func (h *harness) create(agent string, req CreateRequest) (CreateResult, error) {
	if req.Recipient == "" {
		req.Recipient = "me"
	}
	return h.svc.CreateCall(context.Background(), Caller{Agent: agent, Origin: OriginAPI}, req)
}

func (h *harness) mustCreate(agent string, req CreateRequest) models.Call {
	h.t.Helper()
	res, err := h.create(agent, req)
	if err != nil {
		h.t.Fatal(err)
	}
	return res.Call
}

func (h *harness) call(id string) models.Call {
	h.t.Helper()
	c, err := h.store.GetCall(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *harness) report(d models.Device, id, state string) (DeviceCall, error) {
	return h.svc.DeviceReport(context.Background(), d, id, Report{State: state})
}

func (h *harness) mustReport(d models.Device, id, state string) {
	h.t.Helper()
	if _, err := h.report(d, id, state); err != nil {
		h.t.Fatalf("report %s: %v", state, err)
	}
}

func (h *harness) turn(d models.Device, id string, seq int, said string) TurnResponse {
	h.t.Helper()
	r, err := h.svc.Turn(context.Background(), d, id, TurnRequest{Seq: seq, Transcript: said})
	if err != nil {
		h.t.Fatalf("turn %q: %v", said, err)
	}
	return r
}

func (h *harness) tick() { h.svc.Tick(context.Background()) }

// The acceptance scenario: Claude asks for a NORMAL reminder call; the phone
// rings showing Claude; the owner answers, asks for ten more minutes; the
// follow-up is persisted before it is confirmed, and rings ten minutes later.
func TestReminderSnoozeAndCallBack(t *testing.T) {
	h := newHarness(t, afternoon)
	phone := h.enroll("Suzuka")

	c := h.mustCreate("claude", CreateRequest{Type: "REMINDER", Severity: models.SeverityInfo,
		Message: "Wake up. You asked me to remind you to lend me your phone for testing.", UserRequested: true})
	if c.State != models.CallScheduled || c.CallerName != "Claude" || c.Recipient != "pod32g" {
		t.Fatalf("created %+v", c)
	}
	h.tick()
	if got := h.call(c.ID).State; got != models.CallRinging {
		t.Fatalf("after tick: %s, want ringing", got)
	}
	if h.push.count() != 1 || h.push.sent[0].CallerName != "Claude" || h.push.sent[0].CallID != c.ID {
		t.Fatalf("push = %+v", h.push.sent)
	}

	view, err := h.svc.DeviceGet(context.Background(), phone, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The greeting says who is calling; the message follows the pause.
	if view.Greeting.Text != "Hi, this is Gobbie with a reminder from Claude." || view.GreetingPauseMs != 3000 {
		t.Fatalf("greeting = %q, pause %d ms", view.Greeting.Text, view.GreetingPauseMs)
	}
	if !strings.HasPrefix(view.Opening.Text, "Wake up.") || !strings.Contains(view.Opening.Text, "lend me your phone") {
		t.Fatalf("opening = %q", view.Opening.Text)
	}
	if !contains(view.Phrases, "Okay, no problem.") || !contains(view.Phrases, "Claude didn't send any more detail than that.") {
		t.Fatalf("phrases = %q", view.Phrases)
	}

	h.clk.Advance(4 * time.Second)
	h.mustReport(phone, c.ID, "answered")
	h.mustReport(phone, c.ID, "active")

	resp := h.turn(phone, c.ID, 1, "Give me another ten minutes.")
	if resp.Intent != IntentSnooze || !resp.EndCall || resp.Action == nil || resp.Action.Status != "persisted" {
		t.Fatalf("turn = %+v", resp)
	}
	if resp.Reply.Text != "Okay, no problem. I'll call you again in ten minutes, at 4:55 PM. Bye." ||
		resp.Reply.Speech != "Okay, no problem. I'll call you again in ten minutes, at four fifty-five P M. Bye." {
		t.Fatalf("reply = %+v", resp.Reply)
	}
	// Persisted before the reply was returned.
	next := h.call(resp.Action.CallID)
	if next.State != models.CallScheduled || next.ParentCallID != c.ID || next.Origin != OriginUser ||
		!next.ScheduledAt.Equal(h.clk.Now().Add(10*time.Minute)) {
		t.Fatalf("follow-up = %+v", next)
	}
	if got := h.call(c.ID).Outcome; got != models.OutcomeSnoozed {
		t.Fatalf("outcome = %q", got)
	}
	h.mustReport(phone, c.ID, "completed")
	if got := h.call(c.ID).State; got != models.CallCompleted {
		t.Fatalf("state = %s", got)
	}

	// Nothing rings early; then the follow-up rings.
	h.clk.Advance(9 * time.Minute)
	h.tick()
	if h.push.count() != 1 {
		t.Fatalf("rang early: %d pushes", h.push.count())
	}
	h.clk.Advance(time.Minute)
	h.tick()
	if h.push.count() != 2 || h.push.sent[1].CallID != next.ID {
		t.Fatalf("follow-up did not ring: %+v", h.push.sent)
	}
}

func TestTurnReplayDoesNotActTwice(t *testing.T) {
	h := newHarness(t, afternoon)
	phone := h.enroll("Suzuka")
	c := h.mustCreate("claude", CreateRequest{Message: "Stand-up in five."})
	h.tick()
	h.mustReport(phone, c.ID, "answered")
	a := h.turn(phone, c.ID, 1, "ten more minutes")
	b := h.turn(phone, c.ID, 1, "ten more minutes")
	if a.Action == nil || b.Action == nil || a.Action.CallID != b.Action.CallID {
		t.Fatalf("replay differs: %+v vs %+v", a.Action, b.Action)
	}
	scheduled, _ := h.store.ListCalls(context.Background(), storage.CallFilter{States: []models.CallState{models.CallScheduled}})
	if len(scheduled) != 1 {
		t.Fatalf("%d follow-ups, want 1", len(scheduled))
	}
}

func TestConversationIntents(t *testing.T) {
	h := newHarness(t, afternoon)
	phone := h.enroll("Suzuka")
	c := h.mustCreate("claude", CreateRequest{Message: "Your build finished.", Details: "All 412 tests passed."})
	h.tick()
	h.mustReport(phone, c.ID, "answered")
	if r := h.turn(phone, c.ID, 1, "say that again"); r.EndCall || !strings.Contains(r.Reply.Text, "Your build finished.") {
		t.Fatalf("repeat = %+v", r)
	}
	if r := h.turn(phone, c.ID, 2, "tell me more"); r.EndCall || !strings.Contains(r.Reply.Speech, "four hundred and twelve") {
		t.Fatalf("explain = %+v", r)
	}
	if r := h.turn(phone, c.ID, 3, "who is this"); !strings.Contains(r.Reply.Text, "isn't on the line") {
		t.Fatalf("identify = %+v", r)
	}
	if r := h.turn(phone, c.ID, 4, "blue fish"); r.EndCall || r.Intent != IntentUnknown {
		t.Fatalf("unknown = %+v", r)
	}
	if r := h.turn(phone, c.ID, 5, "okay thanks"); !r.EndCall || r.Intent != IntentAcknowledge {
		t.Fatalf("ack = %+v", r)
	}
	if got := h.call(c.ID).Outcome; got != models.OutcomeAcknowledged {
		t.Fatalf("outcome = %q", got)
	}
}

func TestRefusedActionsAreNotConfirmed(t *testing.T) {
	h := newHarness(t, afternoon)
	phone := h.enroll("Suzuka")
	// omni-oncall only offers acknowledge.
	c := h.mustCreate("omni-oncall", CreateRequest{Type: "incident", Severity: models.SeverityCritical,
		Message: "The K3s control plane is unreachable."})
	h.tick()
	h.mustReport(phone, c.ID, "answered")
	r := h.turn(phone, c.ID, 1, "call me back in ten minutes")
	if r.EndCall || r.Action == nil || r.Action.Status != "refused" || strings.Contains(r.Reply.Text, "I'll call you") {
		t.Fatalf("refusal = %+v", r)
	}
	r = h.turn(phone, c.ID, 2, "snooze for 13 hours")
	if r.Action == nil || r.Action.Status != "refused" {
		t.Fatalf("expected refusal, got %+v", r)
	}
	// A claude call may not snooze past max_snooze.
	c2 := h.mustCreate("claude", CreateRequest{Message: "x"})
	h.mustReport(phone, c.ID, "completed")
	h.clk.Advance(2 * time.Minute)
	h.tick()
	h.mustReport(phone, c2.ID, "answered")
	if r := h.turn(phone, c2.ID, 1, "snooze for 13 hours"); r.Action == nil || r.Action.Status != "refused" ||
		!strings.Contains(r.Reply.Text, "twelve hours") {
		t.Fatalf("max snooze = %+v", r)
	}
}

func TestAuthorizationAndValidation(t *testing.T) {
	h := newHarness(t, afternoon)
	if _, err := h.create("stranger", CreateRequest{Message: "hi"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger: %v", err)
	}
	if _, err := h.create("nobody", CreateRequest{Message: "hi"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unknown agent: %v", err)
	}
	if _, err := h.create("gpt", CreateRequest{Message: "hi", Test: true}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("gpt test call: %v", err)
	}
	if _, err := h.create("claude", CreateRequest{Message: ""}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty message: %v", err)
	}
	if _, err := h.create("claude", CreateRequest{Message: "x", Recipient: "someone"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown recipient: %v", err)
	}
	if _, err := h.create("claude", CreateRequest{Message: "x", At: afternoon.Add(-time.Hour)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("past: %v", err)
	}
	// An admin placing a call for claude makes a simulated call.
	res, err := h.svc.CreateCall(context.Background(), Caller{Agent: "claude", Simulated: true, Origin: OriginAPI},
		CreateRequest{Recipient: "me", Message: "Simulated hello", Test: true})
	if err != nil || !res.Call.Simulated || res.Call.CallerName != "Claude (test)" {
		t.Fatalf("simulated: %+v %v", res.Call, err)
	}
	if !strings.Contains(Scripted{}.Greeting(res.Call).Text, "test call") {
		t.Fatalf("test greeting = %q", Scripted{}.Greeting(res.Call).Text)
	}
	// Another agent cannot see or cancel it.
	if _, _, err := h.svc.GetForAgent(context.Background(), "gpt", res.Call.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("gpt sees claude's call: %v", err)
	}
	if _, err := h.svc.Cancel(context.Background(), "gpt", res.Call.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("gpt cancels claude's call: %v", err)
	}
}

func TestIdempotencyAndDuplicates(t *testing.T) {
	h := newHarness(t, afternoon)
	a := h.mustCreate("claude", CreateRequest{Message: "one", IdempotencyKey: "k1"})
	res, err := h.create("claude", CreateRequest{Message: "different text", IdempotencyKey: "k1"})
	if err != nil || !res.Deduplicated || res.Call.ID != a.ID {
		t.Fatalf("idempotent replay: %+v %v", res, err)
	}
	res, err = h.create("claude", CreateRequest{Message: "one"})
	if err != nil || !res.Deduplicated || res.Call.ID != a.ID {
		t.Fatalf("same pending message: %+v %v", res, err)
	}
	// The same key from another agent is a different call.
	res, err = h.create("gpt", CreateRequest{Message: "two", IdempotencyKey: "k1", At: afternoon.Add(time.Hour)})
	if err != nil || res.Deduplicated {
		t.Fatalf("gpt k1: %+v %v", res, err)
	}
}

func TestQuietHours(t *testing.T) {
	// 23:30 in Mexico City.
	night := time.Date(2026, 10, 27, 5, 30, 0, 0, time.UTC)
	h := newHarness(t, night)
	h.enroll("Suzuka")
	morning := time.Date(2026, 10, 27, 8, 0, 0, 0, mexico)

	res, err := h.create("gpt", CreateRequest{Message: "FYI"})
	if err != nil || res.Deferred != "quiet_hours" || !res.Call.ScheduledAt.Equal(morning) {
		t.Fatalf("gpt at night: %+v %v", res, err)
	}
	// claude may ring for a reminder the user asked for (both sides allow it).
	res, err = h.create("claude", CreateRequest{Message: "Take the pills", UserRequested: true})
	if err != nil || res.Deferred != "" {
		t.Fatalf("claude user reminder: %+v %v", res, err)
	}
	// gpt claiming user_requested gets no exception: the grant is per agent.
	res, err = h.create("gpt", CreateRequest{Message: "Trust me", UserRequested: true})
	if err != nil || res.Deferred != "quiet_hours" {
		t.Fatalf("gpt claiming user request: %+v %v", res, err)
	}
	// critical from omni-oncall rings; critical from gpt waits.
	res, err = h.create("omni-oncall", CreateRequest{Message: "Down", Severity: models.SeverityCritical})
	if err != nil || res.Deferred != "" {
		t.Fatalf("oncall critical: %+v %v", res, err)
	}
	res, err = h.create("gpt", CreateRequest{Message: "Very important", Severity: models.SeverityCritical})
	if err != nil || res.Deferred != "quiet_hours" {
		t.Fatalf("gpt critical: %+v %v", res, err)
	}
	// A call scheduled into quiet hours is deferred when due.
	c := h.mustCreate("gpt", CreateRequest{Message: "later", At: night.Add(30 * time.Minute)})
	h.clk.Advance(31 * time.Minute)
	h.tick()
	if got := h.call(c.ID); got.State != models.CallScheduled || !got.ScheduledAt.Equal(morning) {
		t.Fatalf("scheduled into quiet hours: %+v", got)
	}
}

func TestQuietHoursWindow(t *testing.T) {
	u, _ := mustConfig(t).Voice.User("me")
	at := func(h, m int) time.Time { return time.Date(2026, 10, 26, h, m, 0, 0, mexico) }
	for _, c := range []struct {
		t     time.Time
		quiet bool
	}{
		{at(22, 59), false}, {at(23, 0), true}, {at(2, 0), true}, {at(7, 59), true}, {at(8, 0), false}, {at(12, 0), false},
	} {
		end, q := quietUntil(u, c.t)
		if q != c.quiet {
			t.Errorf("%s quiet=%v want %v", c.t.Format("15:04"), q, c.quiet)
		}
		if q && (end.In(mexico).Hour() != 8 || end.Sub(c.t) > 9*time.Hour) {
			t.Errorf("%s quiet ends %s", c.t.Format("15:04"), end.In(mexico))
		}
	}
}

func mustConfig(t *testing.T) config.Config {
	cfg, err := config.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestRateLimitAndCooldown(t *testing.T) {
	h := newHarness(t, afternoon)
	phone := h.enroll("Suzuka")
	ring := func(agent, msg string) error {
		res, err := h.create(agent, CreateRequest{Message: msg})
		if err != nil {
			return err
		}
		h.tick()
		h.mustReport(phone, res.Call.ID, "declined")
		return nil
	}
	if err := ring("claude", "1"); err != nil {
		t.Fatal(err)
	}
	var rl *RateLimitedError
	if err := ring("claude", "2"); !errors.As(err, &rl) || rl.Reason != "cooldown" {
		t.Fatalf("cooldown: %v", err)
	}
	if err := ring("gpt", "3"); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(2 * time.Minute)
	if err := ring("claude", "4"); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(2 * time.Minute)
	if err := ring("gpt", "5"); !errors.As(err, &rl) || rl.Reason != "rate_limit" {
		t.Fatalf("rate limit: %v", err)
	}
	if rl.RetryAfter <= 0 || rl.RetryAfter > time.Hour {
		t.Fatalf("retry after %s", rl.RetryAfter)
	}
}

func TestMissedCallRetriesThenFallsBack(t *testing.T) {
	h := newHarness(t, afternoon)
	h.enroll("Suzuka")
	c := h.mustCreate("claude", CreateRequest{Message: "Ping"})
	h.tick()
	h.clk.Advance(46 * time.Second)
	h.tick() // sweeper: ring deadline passed
	if got := h.call(c.ID).State; got != models.CallMissed {
		t.Fatalf("state %s, want missed", got)
	}
	retries, _ := h.store.ListCalls(context.Background(), storage.CallFilter{States: []models.CallState{models.CallScheduled}})
	if len(retries) != 1 || retries[0].Attempt != 2 || retries[0].ParentCallID != c.ID || retries[0].Origin != OriginRetry {
		t.Fatalf("retry = %+v", retries)
	}
	if len(h.fallback.sent) != 0 {
		t.Fatal("fallback before retries ran out")
	}
	h.clk.Advance(2 * time.Minute)
	h.tick()
	if got := h.call(retries[0].ID).State; got != models.CallRinging {
		t.Fatalf("retry state %s", got)
	}
	h.clk.Advance(46 * time.Second)
	h.tick()
	if len(h.fallback.sent) != 1 || h.fallback.sent[0] != c.EventRef {
		t.Fatalf("fallback = %v", h.fallback.sent)
	}
}

func TestDeclinedIsNotRetried(t *testing.T) {
	h := newHarness(t, afternoon)
	phone := h.enroll("Suzuka")
	c := h.mustCreate("claude", CreateRequest{Message: "Ping"})
	h.tick()
	h.mustReport(phone, c.ID, "declined")
	scheduled, _ := h.store.ListCalls(context.Background(), storage.CallFilter{States: []models.CallState{models.CallScheduled}})
	if len(scheduled) != 0 || len(h.fallback.sent) != 0 {
		t.Fatalf("declined call retried: %+v %v", scheduled, h.fallback.sent)
	}
	// A late duplicate report is harmless; a contradicting one is refused.
	if _, err := h.report(phone, c.ID, "declined"); err != nil {
		t.Fatalf("duplicate report: %v", err)
	}
	if _, err := h.report(phone, c.ID, "answered"); !errors.Is(err, ErrState) {
		t.Fatalf("answer after decline: %v", err)
	}
}

func TestTwoDevicesFirstAnswerWins(t *testing.T) {
	h := newHarness(t, afternoon)
	a, b := h.enroll("Suzuka"), h.enroll("iPhone Air")
	c := h.mustCreate("claude", CreateRequest{Message: "Ping"})
	h.tick()
	if h.push.count() != 2 {
		t.Fatalf("pushes = %d", h.push.count())
	}
	h.mustReport(a, c.ID, "answered")
	v, err := h.report(b, c.ID, "answered")
	if !errors.Is(err, ErrState) || v.State != models.CallAnswered || v.AnsweredHere {
		t.Fatalf("second answer: %+v %v", v, err)
	}
	if _, err := h.svc.Turn(context.Background(), b, c.ID, TurnRequest{Seq: 1, Transcript: "ok"}); !errors.Is(err, ErrState) {
		t.Fatalf("turn from the other phone: %v", err)
	}
	inv, _ := h.svc.Ringing(context.Background(), b)
	if len(inv) != 0 {
		t.Fatalf("still ringing on b: %+v", inv)
	}
}

func TestRestartRingsOnce(t *testing.T) {
	h := newHarness(t, afternoon)
	h.enroll("Suzuka")
	c := h.mustCreate("claude", CreateRequest{Message: "At five", At: afternoon.Add(15 * time.Minute)})
	h.store.Close()

	// A new process with the same database and no memory of the call.
	h.open()
	h.clk.Advance(15 * time.Minute)
	h.tick()
	h.tick()
	h.open()
	h.tick()
	if h.push.count() != 1 || h.call(c.ID).State != models.CallRinging {
		t.Fatalf("pushes %d, state %s", h.push.count(), h.call(c.ID).State)
	}
}

func TestExpiryNoDeviceAndCancel(t *testing.T) {
	h := newHarness(t, afternoon)
	// No device yet.
	c := h.mustCreate("claude", CreateRequest{Message: "Nobody home"})
	h.tick()
	if got := h.call(c.ID); got.State != models.CallFailed || got.Reason != "no enrolled device" {
		t.Fatalf("no device: %+v", got)
	}
	phone := h.enroll("Suzuka")

	// Cancelled before it is due: never rings.
	c = h.mustCreate("claude", CreateRequest{Message: "Cancel me", At: afternoon.Add(time.Hour)})
	if _, err := h.svc.Cancel(context.Background(), "claude", c.ID); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(time.Hour)
	h.tick()
	if h.push.count() != 0 {
		t.Fatal("cancelled call rang")
	}
	if _, err := h.svc.Cancel(context.Background(), "claude", c.ID); !errors.Is(err, ErrState) {
		t.Fatalf("cancel twice: %v", err)
	}

	// Busy, then deferred past its expiry by the busy line: expires.
	busy := h.mustCreate("claude", CreateRequest{Message: "First"})
	h.tick()
	h.mustReport(phone, busy.ID, "answered")
	short := h.mustCreate("gpt", CreateRequest{Message: "Second", ExpiresIn: 20 * time.Second})
	h.tick()
	if got := h.call(short.ID).State; got != models.CallExpired {
		t.Fatalf("short-lived call while busy: %s", got)
	}
}

func TestNonInteractiveAndStaleCalls(t *testing.T) {
	h := newHarness(t, afternoon)
	phone := h.enroll("Suzuka")
	no := false
	c := h.mustCreate("claude", CreateRequest{Message: "Just so you know.", Interactive: &no})
	h.tick()
	h.mustReport(phone, c.ID, "answered")
	if _, err := h.svc.Turn(context.Background(), phone, c.ID, TurnRequest{Seq: 1, Transcript: "ok"}); !errors.Is(err, ErrState) {
		t.Fatalf("turn on a non-interactive call: %v", err)
	}
	view, _ := h.svc.DeviceGet(context.Background(), phone, c.ID)
	if !strings.HasSuffix(view.Opening.Text, "Goodbye.") {
		t.Fatalf("opening = %q", view.Opening.Text)
	}
	// The phone vanishes mid-call: the sweeper fails the call.
	h.clk.Advance(6 * time.Minute)
	h.tick()
	if got := h.call(c.ID).State; got != models.CallFailed {
		t.Fatalf("stale call state %s", got)
	}
}

func TestPushFailures(t *testing.T) {
	h := newHarness(t, afternoon)
	h.enroll("Suzuka")
	h.push.err = ErrBadDeviceToken
	c := h.mustCreate("claude", CreateRequest{Message: "Ping"})
	h.tick()
	if got := h.call(c.ID); got.State != models.CallFailed {
		t.Fatalf("all pushes failed: %s", got.State)
	}
	devs, _ := h.store.ListDevices(context.Background(), "pod32g", true)
	if devs[0].HasVoIPToken {
		t.Fatal("rejected token was not cleared")
	}
	// Without a token the device is invited by polling (development).
	h.push.err = nil
	h.clk.Advance(2 * time.Minute)
	c = h.mustCreate("claude", CreateRequest{Message: "Ping again", IdempotencyKey: "2"})
	h.tick()
	inv, _ := h.svc.Ringing(context.Background(), devs[0])
	if h.call(c.ID).State != models.CallRinging || len(inv) != 1 || inv[0].CallID != c.ID {
		t.Fatalf("poll invitation: %+v", inv)
	}
}

func TestEnrollmentCodesAreSingleUse(t *testing.T) {
	h := newHarness(t, afternoon)
	code, _, err := h.svc.CreateEnrollment(context.Background(), "me")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.Enroll(context.Background(), EnrollRequest{Code: code}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.Enroll(context.Background(), EnrollRequest{Code: code}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reuse: %v", err)
	}
	code, _, _ = h.svc.CreateEnrollment(context.Background(), "me")
	h.clk.Advance(16 * time.Minute)
	if _, _, err := h.svc.Enroll(context.Background(), EnrollRequest{Code: code}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expired: %v", err)
	}
	if _, _, err := h.svc.Enroll(context.Background(), EnrollRequest{Code: "x", VoIPToken: "not hex!"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad token: %v", err)
	}
}

func TestStateMachine(t *testing.T) {
	allowed := map[[2]models.CallState]bool{}
	for from, tos := range map[models.CallState][]models.CallState{
		models.CallScheduled: {models.CallRinging, models.CallCancelled, models.CallExpired, models.CallFailed},
		models.CallRinging:   {models.CallAnswered, models.CallDeclined, models.CallMissed, models.CallCancelled, models.CallFailed},
		models.CallAnswered:  {models.CallActive, models.CallCompleted, models.CallFailed},
		models.CallActive:    {models.CallCompleted, models.CallFailed},
	} {
		for _, to := range tos {
			allowed[[2]models.CallState{from, to}] = true
		}
	}
	all := []models.CallState{models.CallScheduled, models.CallRinging, models.CallAnswered, models.CallActive,
		models.CallCompleted, models.CallDeclined, models.CallMissed, models.CallExpired, models.CallFailed, models.CallCancelled}
	for _, from := range all {
		for _, to := range all {
			if got := models.CanTransition(from, to); got != allowed[[2]models.CallState{from, to}] {
				t.Errorf("%s → %s = %v", from, to, got)
			}
		}
	}
	for _, s := range []models.CallState{models.CallCompleted, models.CallDeclined, models.CallMissed, models.CallExpired,
		models.CallFailed, models.CallCancelled} {
		if !s.Terminal() {
			t.Errorf("%s not terminal", s)
		}
	}
}

// Every reply starts with a sentence the phone has rendered in advance.
func TestRepliesStartWithAPhrase(t *testing.T) {
	c := models.Call{CallerName: "Claude", Message: "Ping.", Interactive: true}
	phrases := Scripted{}.Phrases(c)
	at := now.Add(10 * time.Minute)
	for _, in := range []Intent{{Kind: IntentAcknowledge}, {Kind: IntentDismiss}, {Kind: IntentSnooze, After: 10 * time.Minute},
		{Kind: IntentReschedule, At: at}, {Kind: IntentEnd}, {Kind: IntentSilence}, {Kind: IntentUnknown}, {Kind: IntentIdentify},
		{Kind: IntentExplain}} {
		for misses := 0; misses < 3; misses++ {
			u, _ := Scripted{}.Reply(c, in, ActionResult{Done: true, At: at}, misses, now, mexico)
			first := u.Speech
			if i := strings.IndexAny(first, ".?!"); i >= 0 {
				first = first[:i+1]
			}
			if !contains(phrases, first) {
				t.Errorf("%s (misses %d) starts with %q, not a phrase", in.Kind, misses, first)
			}
		}
	}
}
