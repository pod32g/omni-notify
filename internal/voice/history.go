package voice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/storage"
)

// A phone's own calls, and calling back from the phone: the Calls tab of the
// On-Call app (omni-notify-ios docs/calls/API.md, C1, C2 and C4).

// OriginPhone marks a call the recipient started from the phone (call back).
const OriginPhone = "phone"

// ErrUnprocessable is a request that is well formed but can't be done, such
// as calling back a test call.
var ErrUnprocessable = errors.New("cannot be done")

// CallRecord is one call in a phone's history (C1).
type CallRecord struct {
	ID            string               `json:"id"`
	CallerName    string               `json:"caller_name"`
	Agent         string               `json:"agent"`
	Title         string               `json:"title"`
	State         models.CallState     `json:"state"`
	Outcome       models.CallOutcome   `json:"outcome"`
	Reason        string               `json:"reason"`
	AnsweredHere  bool                 `json:"answered_here"`
	UserRequested bool                 `json:"user_requested"`
	RangAt        *time.Time           `json:"rang_at"`
	AnsweredAt    *time.Time           `json:"answered_at"`
	EndedAt       *time.Time           `json:"ended_at"`
	FollowUpAt    *time.Time           `json:"follow_up_at"`
	Incident      *models.CallIncident `json:"incident"`
	TurnCount     int                  `json:"turn_count"`
}

// HistoryTurn is one exchange as the phone shows it afterwards (C2).
type HistoryTurn struct {
	Seq    int       `json:"seq"`
	At     time.Time `json:"at"`
	Heard  string    `json:"heard"` // empty unless voice.store_transcripts is on
	Said   string    `json:"said"`
	Intent string    `json:"intent"`
}

// CallDetail is one call with what was said (C2).
type CallDetail struct {
	CallRecord
	Greeting Utterance     `json:"greeting"`
	Opening  Utterance     `json:"opening"`
	Turns    []HistoryTurn `json:"turns"`
}

// DeviceHistory lists the calls to the device's person, newest first: every
// call that has happened, including those answered on another phone. next is
// the cursor for the following page, nil at the end.
func (s *Service) DeviceHistory(ctx context.Context, d models.Device, limit int, before time.Time) ([]CallRecord, *time.Time, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	calls, err := s.store.CallHistory(ctx, d.User, before, limit)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]string, len(calls))
	for i, c := range calls {
		ids[i] = c.ID
	}
	follow, err := s.store.FollowUpTimes(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	turns, err := s.store.TurnCounts(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	out := make([]CallRecord, 0, len(calls))
	for _, c := range calls {
		r := record(c, d)
		if at, ok := follow[c.ID]; ok && (c.Outcome == models.OutcomeSnoozed || c.Outcome == models.OutcomeRescheduled) {
			r.FollowUpAt = &at
		}
		r.TurnCount = turns[c.ID]
		out = append(out, r)
	}
	var next *time.Time
	if len(calls) == limit {
		at := happenedAt(calls[len(calls)-1])
		next = &at
	}
	return out, next, nil
}

// DeviceHistoryCall returns one of the person's calls with its conversation.
func (s *Service) DeviceHistoryCall(ctx context.Context, d models.Device, id string) (CallDetail, error) {
	c, err := s.store.GetCall(ctx, id)
	if errors.Is(err, storage.ErrNotFound) || (err == nil && c.Recipient != d.User) {
		return CallDetail{}, ErrNotFound
	}
	if err != nil {
		return CallDetail{}, err
	}
	stored, err := s.store.CallTurns(ctx, id)
	if err != nil {
		return CallDetail{}, err
	}
	out := CallDetail{CallRecord: record(c, d), Greeting: s.responder.Greeting(c), Opening: s.responder.Opening(c),
		Turns: make([]HistoryTurn, 0, len(stored))}
	out.TurnCount = len(stored)
	for _, t := range stored {
		ht := HistoryTurn{Seq: t.Seq, At: t.At, Heard: t.Transcript, Intent: t.Intent}
		if resp, err := decodeTurn(t.Turn); err == nil {
			ht.Said = resp.Reply.Text
		}
		out.Turns = append(out.Turns, ht)
	}
	if c.Outcome == models.OutcomeSnoozed || c.Outcome == models.OutcomeRescheduled {
		if follow, err := s.store.FollowUpTimes(ctx, []string{id}); err == nil {
			if at, ok := follow[id]; ok {
				out.FollowUpAt = &at
			}
		}
	}
	return out, nil
}

func record(c models.Call, d models.Device) CallRecord {
	return CallRecord{
		ID: c.ID, CallerName: c.CallerName, Agent: c.Agent, Title: displayTitle(c), State: c.State, Outcome: c.Outcome,
		Reason: c.Reason, AnsweredHere: c.AnsweredBy != "" && c.AnsweredBy == d.ID, UserRequested: c.UserRequested,
		RangAt: c.RangAt, AnsweredAt: c.AnsweredAt, EndedAt: c.EndedAt, Incident: c.Incident,
	}
}

// happenedAt matches storage's ordering of a history (when it rang, else when
// it was answered or ended).
func happenedAt(c models.Call) time.Time {
	for _, t := range []*time.Time{c.RangAt, c.AnsweredAt, c.EndedAt} {
		if t != nil {
			return *t
		}
	}
	return c.CreatedAt
}

// displayTitle is what a call was about in a few words: its title, or the
// first sentence of its message when it has none.
func displayTitle(c models.Call) string {
	if t := strings.TrimSpace(c.Title); t != "" {
		return t
	}
	m := strings.TrimSpace(c.Message)
	if i := strings.IndexByte(m, '\n'); i > 0 {
		m = m[:i]
	}
	// The first sentence: a stop followed by a space ("1.1.9" is not one).
	for i := 0; i+1 < len(m); i++ {
		if strings.IndexByte(".!?", m[i]) >= 0 && m[i+1] == ' ' && i > 0 {
			m = m[:i]
			break
		}
	}
	m = strings.TrimRight(m, ".!?")
	if r := []rune(m); len(r) > 80 {
		m = strings.TrimSpace(string(r[:79])) + "…"
	}
	return m
}

// CallBack makes a new call about an earlier one, started by the phone (C4):
// the same agent, message and incident, already answered by this device, so
// it does not ring and quiet hours do not apply (the person asked). A
// repeated idempotency key returns the call already made.
func (s *Service) CallBack(ctx context.Context, d models.Device, regarding, key string) (DeviceCall, bool, error) {
	if len(key) > 200 {
		return DeviceCall{}, false, invalid("Idempotency-Key is too long")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	from, err := s.store.GetCall(ctx, regarding)
	if errors.Is(err, storage.ErrNotFound) || (err == nil && from.Recipient != d.User) {
		return DeviceCall{}, false, ErrNotFound
	}
	if err != nil {
		return DeviceCall{}, false, err
	}
	idem := ""
	if key != "" {
		idem = "callback:" + d.ID + ":" + key
		if prev, err := s.store.CallByIdempotencyKey(ctx, from.Agent, idem); err == nil {
			return s.deviceView(prev, d), false, nil
		}
	}
	if from.Test || from.Simulated {
		return DeviceCall{}, false, fmt.Errorf("%w: a test call can't be called back", ErrUnprocessable)
	}
	user, okU := s.cfg.User(from.Recipient)
	agent, okA := s.cfg.Agent(from.Agent)
	if !okU || !okA || !contains(user.AllowedAgents, agent.ID) {
		return DeviceCall{}, false, fmt.Errorf("%w: %s can no longer call you", ErrUnprocessable, from.CallerName)
	}
	live, err := s.store.LiveCalls(ctx, user.ID)
	if err != nil {
		return DeviceCall{}, false, err
	}
	if len(live) > 0 {
		return DeviceCall{}, false, ErrState
	}

	now := s.now()
	c := from
	c.ID, c.State, c.Outcome, c.Reason = newID(), models.CallAnswered, models.OutcomeNone, "call back"
	c.Origin, c.Attempt, c.ParentCallID, c.UserRequested, c.Interactive = OriginPhone, 1, from.ID, true, true
	c.IdempotencyKey, c.CallerName = idem, agent.DisplayName
	c.ScheduledAt, c.ExpiresAt = now, now.Add(s.cfg.DefaultExpiry.D())
	c.RingDeadline, c.RangAt, c.EndedAt = nil, nil, nil
	c.AnsweredAt, c.AnsweredBy = &now, d.ID
	c.LastActivityAt, c.CreatedAt, c.UpdatedAt = now, now, now
	stored, err := s.store.InsertCall(ctx, c, "device:"+d.ID, "call back of "+from.ID)
	if errors.Is(err, storage.ErrConflict) {
		return s.deviceView(stored, d), false, nil
	}
	if err != nil {
		return DeviceCall{}, false, err
	}
	// The device may only act on calls it was offered: this one it started.
	if err := s.store.RecordRing(ctx, c.ID, d.ID, "phone", ""); err != nil {
		return DeviceCall{}, false, err
	}
	s.metrics.VoiceCalls.WithLabelValues(OriginPhone).Inc()
	s.log.Info("voice call back", "call", c.ID, "of", from.ID, "device", d.ID, "agent", c.Agent)
	return s.deviceView(stored, d), true, nil
}

// incidentFromEvent reads the on-call plugin's page (type oncall.page, label
// oncall_incident, annotation incident_number) so a call placed for it knows
// its incident (C3).
func incidentFromEvent(ev models.Event) *models.CallIncident {
	id := ev.Labels["oncall_incident"]
	if id == "" {
		return nil
	}
	in := &models.CallIncident{ID: id, Title: ev.Title, Severity: ev.Labels["oncall_severity"]}
	if n, err := json.Number(ev.Annotations["incident_number"]).Int64(); err == nil {
		in.Number = int(n)
	}
	return in
}
