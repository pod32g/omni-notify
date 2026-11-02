package voice

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
)

var page128 = &models.CallIncident{ID: "inc_128", Number: 128, Title: "Jellyfin streams failing with 5xx above 20%", Severity: "sev1"}

// ringAnswer rings a just-created call and answers it on phone.
func (h *harness) ringAnswer(phone models.Device, id string) {
	h.t.Helper()
	h.tick()
	h.clk.Advance(3 * time.Second)
	h.mustReport(phone, id, "answered")
	h.mustReport(phone, id, "active")
}

// The Calls tab: every call to the person that happened, newest first, with
// what came of it, and nothing that hasn't rung yet.
func TestHistoryForThePhone(t *testing.T) {
	h := newHarness(t, afternoon)
	h.svc.cfg.StoreTranscripts = true
	phone, pad := h.enroll("Suzuka"), h.enroll("iPad")
	ctx := context.Background()

	page := h.mustCreate("omni-oncall", CreateRequest{Severity: models.SeverityCritical, Title: page128.Title,
		Message: "Jellyfin streams are failing.", Incident: page128})
	h.ringAnswer(phone, page.ID)
	if r := h.turn(phone, page.ID, 1, "acknowledge"); r.Intent != IntentAcknowledge {
		t.Fatalf("turn = %+v", r)
	}
	h.clk.Advance(30 * time.Second)
	h.mustReport(phone, page.ID, "completed")

	h.clk.Advance(5 * time.Minute)
	reminder := h.mustCreate("claude", CreateRequest{Type: "reminder", Message: "Release build 1.1.9 is ready to test. Try it on the Pad."})
	h.ringAnswer(pad, reminder.ID)
	snooze := h.turn(pad, reminder.ID, 1, "snooze for ten minutes")
	h.mustReport(pad, reminder.ID, "completed")

	h.clk.Advance(5 * time.Minute)
	missed := h.mustCreate("gpt", CreateRequest{Message: "Your build finished."})
	h.tick()
	h.clk.Advance(time.Minute)
	h.tick() // past the ring deadline: missed, and a retry is scheduled

	got, next, err := h.svc.DeviceHistory(ctx, phone, 50, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if next != nil {
		t.Fatalf("next = %v on a short list", next)
	}
	if len(got) != 3 || got[0].ID != missed.ID || got[1].ID != reminder.ID || got[2].ID != page.ID {
		t.Fatalf("history = %+v", got)
	}
	m, r, p := got[0], got[1], got[2]
	if m.State != models.CallMissed || m.AnsweredAt != nil || m.RangAt == nil || m.EndedAt == nil || m.FollowUpAt != nil {
		t.Errorf("missed = %+v", m)
	}
	// Answered on the iPad: listed on the phone, but not as answered here. The
	// follow-up's time comes with it, and a title from the message.
	if r.AnsweredHere || r.Outcome != models.OutcomeSnoozed || r.FollowUpAt == nil ||
		!r.FollowUpAt.Equal(*snooze.Action.At) || r.Title != "Release build 1.1.9 is ready to test" || r.TurnCount != 1 {
		t.Errorf("reminder = %+v", r)
	}
	if !p.AnsweredHere || p.Outcome != models.OutcomeAcknowledged || p.Incident == nil || *p.Incident != *page128 ||
		p.TurnCount != 1 || p.CallerName != "Omni Oncall" {
		t.Errorf("page = %+v", p)
	}

	// Pages: the cursor is when the last call happened.
	first, next, err := h.svc.DeviceHistory(ctx, phone, 2, time.Time{})
	if err != nil || len(first) != 2 || next == nil {
		t.Fatalf("page 1 = %d calls, next %v, %v", len(first), next, err)
	}
	rest, next, err := h.svc.DeviceHistory(ctx, phone, 2, *next)
	if err != nil || len(rest) != 1 || rest[0].ID != page.ID || next != nil {
		t.Fatalf("page 2 = %+v, next %v, %v", rest, next, err)
	}

	// One call with what was said.
	d, err := h.svc.DeviceHistoryCall(ctx, phone, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Greeting.Text == "" || d.Opening.Text == "" || len(d.Turns) != 1 || d.Turns[0].Heard != "acknowledge" ||
		d.Turns[0].Said == "" || d.Turns[0].Intent != string(IntentAcknowledge) || d.Incident == nil {
		t.Fatalf("detail = %+v", d)
	}
	if _, err := h.svc.DeviceHistoryCall(ctx, phone, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown call: %v", err)
	}
}

// Without voice.store_transcripts, the detail keeps Gobbie's side only.
func TestHistoryDetailWithoutTranscripts(t *testing.T) {
	h := newHarness(t, afternoon)
	phone := h.enroll("Suzuka")
	c := h.mustCreate("claude", CreateRequest{Message: "Ping."})
	h.ringAnswer(phone, c.ID)
	h.turn(phone, c.ID, 1, "what?")
	d, err := h.svc.DeviceHistoryCall(context.Background(), phone, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Turns) != 1 || d.Turns[0].Heard != "" || d.Turns[0].Said == "" {
		t.Fatalf("turns = %+v", d.Turns)
	}
}

// Call back: a new call about the same thing, answered by the phone that
// asked, which can then talk on it as on any answered call.
func TestCallBackFromThePhone(t *testing.T) {
	h := newHarness(t, afternoon)
	phone := h.enroll("Suzuka")
	ctx := context.Background()

	page := h.mustCreate("omni-oncall", CreateRequest{Severity: models.SeverityCritical, Title: page128.Title,
		Message: "Jellyfin streams are failing.", Incident: page128})
	h.tick()
	h.clk.Advance(time.Minute)
	h.tick() // missed

	h.clk.Advance(time.Minute)
	v, created, err := h.svc.CallBack(ctx, phone, page.ID, "key-1")
	if err != nil || !created {
		t.Fatalf("call back: %v (created %v)", err, created)
	}
	if v.State != models.CallAnswered || !v.AnsweredHere || v.Incident == nil || v.Incident.ID != "inc_128" ||
		v.Greeting.Text != "Hi, it's Gobbie, calling you back for Omni Oncall." || !v.Interactive {
		t.Fatalf("view = %+v", v)
	}
	c := h.call(v.ID)
	if c.ParentCallID != page.ID || c.Origin != OriginPhone || !c.UserRequested || c.RangAt != nil {
		t.Fatalf("call = %+v", c)
	}
	// The same key returns the same call; a second call while this one is on
	// the line is refused.
	again, created, err := h.svc.CallBack(ctx, phone, page.ID, "key-1")
	if err != nil || created || again.ID != v.ID {
		t.Fatalf("replay: %+v %v %v", again, created, err)
	}
	if _, _, err := h.svc.CallBack(ctx, phone, page.ID, "key-2"); !errors.Is(err, ErrState) {
		t.Fatalf("while on a call: %v", err)
	}

	// It runs like any answered call.
	h.mustReport(phone, v.ID, "active")
	if r := h.turn(phone, v.ID, 1, "acknowledge"); r.Intent != IntentAcknowledge || r.Action == nil {
		t.Fatalf("turn = %+v", r)
	}
	h.mustReport(phone, v.ID, "completed")

	got, _, err := h.svc.DeviceHistory(ctx, phone, 50, time.Time{})
	if err != nil || len(got) < 2 || got[0].ID != v.ID || !got[0].UserRequested || got[0].RangAt != nil || got[0].AnsweredAt == nil {
		t.Fatalf("history = %+v, %v", got, err)
	}

	// Not someone else's call, and not a test call.
	if _, _, err := h.svc.CallBack(ctx, phone, "nope", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown call: %v", err)
	}
	test := h.mustCreate("claude", CreateRequest{Message: "Test.", Test: true})
	if _, _, err := h.svc.CallBack(ctx, phone, test.ID, ""); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("test call: %v", err)
	}
}

// A page routed to a voice_call provider knows its incident from the
// on-call plugin's labels.
func TestIncidentFromAPage(t *testing.T) {
	ev := models.Event{Type: "oncall.page", Title: "Jellyfin streams failing with 5xx above 20%",
		Labels:      map[string]string{"oncall_incident": "inc_128", "oncall_severity": "sev1", "service": "jellyfin"},
		Annotations: map[string]string{"incident_number": "128"}}
	if in := incidentFromEvent(ev); in == nil || *in != *page128 {
		t.Fatalf("incident = %+v", in)
	}
	if in := incidentFromEvent(models.Event{Title: "Backup failed"}); in != nil {
		t.Fatalf("plain event has an incident: %+v", in)
	}
}
