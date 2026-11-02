package voice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/pod32g/omni-notify/internal/clock"
	"github.com/pod32g/omni-notify/internal/config"
	"github.com/pod32g/omni-notify/internal/dedupe"
	"github.com/pod32g/omni-notify/internal/metrics"
	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/storage"
)

// Where a call came from.
const (
	OriginAPI   = "api"   // an agent or service asked for it
	OriginRoute = "route" // an event was routed to a voice_call provider
	OriginUser  = "user"  // the recipient asked for it on a call (snooze/reschedule)
	OriginRetry = "retry" // the previous attempt was missed
)

// Errors the API maps to status codes.
var (
	ErrInvalid   = errors.New("invalid request")
	ErrForbidden = errors.New("forbidden")
	ErrNotFound  = errors.New("not found")
	ErrState     = errors.New("call is not in a state that allows this")
)

// RateLimitedError says when a refused immediate call could be placed.
type RateLimitedError struct {
	Reason     string
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("rate limited (%s); retry in %s", e.Reason, e.RetryAfter.Round(time.Second))
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Fallback delivers a call's notification through an ordinary provider when
// the call ends unanswered.
type Fallback interface {
	SendFallback(ctx context.Context, eventRef int64, provider string) error
}

// Service runs the VOICE_CALL channel.
type Service struct {
	cfg       config.VoiceConfig
	store     *storage.Store
	pusher    Pusher
	responder Responder
	fallback  Fallback
	metrics   *metrics.Metrics
	clock     clock.Clock
	log       *slog.Logger
	poll      time.Duration

	wake   chan struct{}
	doneCh chan struct{}
	mu     sync.Mutex // serialises dispatch passes against device reports
}

// Options wires optional collaborators.
type Options struct {
	Pusher    Pusher    // nil: no push; devices poll (development)
	Responder Responder // nil: Scripted
	Fallback  Fallback
	Clock     clock.Clock
	Log       *slog.Logger
	Poll      time.Duration
}

// NewService builds the voice service. cfg must have passed config validation.
func NewService(cfg config.VoiceConfig, store *storage.Store, m *metrics.Metrics, o Options) *Service {
	if o.Responder == nil {
		o.Responder = Scripted{}
	}
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Poll <= 0 {
		o.Poll = time.Second
	}
	return &Service{cfg: cfg, store: store, pusher: o.Pusher, responder: o.Responder, fallback: o.Fallback,
		metrics: m, clock: o.Clock, log: o.Log, poll: o.Poll, wake: make(chan struct{}, 1)}
}

// Config returns the voice configuration.
func (s *Service) Config() config.VoiceConfig { return s.cfg }

// PushConfigured reports whether calls are pushed (else devices must poll).
func (s *Service) PushConfigured() bool { return s.pusher != nil }

func (s *Service) now() time.Time { return s.clock.Now().UTC() }

// CreateRequest is a request to call someone.
type CreateRequest struct {
	Recipient      string
	Type           string
	Severity       models.Severity
	Title          string
	Message        string
	Details        string
	At             time.Time     // zero: now (or After from now)
	After          time.Duration // alternative to At
	ExpiresIn      time.Duration // after the scheduled time; 0: config default
	Interactive    *bool         // nil: true
	UserRequested  bool
	IdempotencyKey string
	Test           bool
	// Incident is the on-call incident the call is placed for, if any.
	Incident *models.CallIncident
}

// Caller is the authenticated party placing a call.
type Caller struct {
	Agent     string // agent id, from authentication or a trusted mapping
	Simulated bool   // an admin placing a call as the agent
	Origin    string
	EventRef  int64 // an existing event (route origin); 0 creates one
}

// CreateResult reports what CreateCall did.
type CreateResult struct {
	Call         models.Call `json:"call"`
	Created      bool        `json:"created"`
	Deduplicated bool        `json:"deduplicated"`
	Deferred     string      `json:"deferred,omitempty"`
}

const (
	maxMessage = 1000
	maxDetails = 2000
	maxTitle   = 200
)

// CreateCall validates a request against the caller's rights and the
// recipient's policy and stores the call, scheduled now or later.
func (s *Service) CreateCall(ctx context.Context, caller Caller, req CreateRequest) (CreateResult, error) {
	req.Message = strings.TrimSpace(req.Message)
	req.Type = strings.ToLower(strings.TrimSpace(req.Type))
	if req.Type == "" {
		req.Type = "notification"
	}
	switch {
	case req.Message == "":
		return CreateResult{}, invalid("message is required")
	case len(req.Message) > maxMessage:
		return CreateResult{}, invalid("message is longer than %d characters", maxMessage)
	case len(req.Details) > maxDetails:
		return CreateResult{}, invalid("details are longer than %d characters", maxDetails)
	case len(req.Title) > maxTitle:
		return CreateResult{}, invalid("title is longer than %d characters", maxTitle)
	case !req.Severity.Valid():
		return CreateResult{}, invalid("invalid severity %q", req.Severity)
	case len(req.IdempotencyKey) > 200:
		return CreateResult{}, invalid("idempotency_key is too long")
	}
	if in := req.Incident; in != nil {
		in.ID, in.Title, in.Severity = strings.TrimSpace(in.ID), strings.TrimSpace(in.Title), strings.TrimSpace(in.Severity)
		switch {
		case in.ID == "" || len(in.ID) > 100:
			return CreateResult{}, invalid("incident.id is required and at most 100 characters")
		case in.Number < 0, len(in.Title) > maxTitle, len(in.Severity) > 20:
			return CreateResult{}, invalid("incident has an invalid number, title or severity")
		}
	}
	user, ok := s.cfg.User(req.Recipient)
	if !ok {
		return CreateResult{}, invalid("unknown recipient %q", req.Recipient)
	}
	agent, ok := s.cfg.Agent(caller.Agent)
	if !ok {
		return CreateResult{}, fmt.Errorf("%w: agent %q is not configured for voice calls", ErrForbidden, caller.Agent)
	}
	if !contains(user.AllowedAgents, agent.ID) {
		s.metrics.VoiceSchedulingFailure.WithLabelValues("agent_not_allowed").Inc()
		return CreateResult{}, fmt.Errorf("%w: %s may not call %s", ErrForbidden, agent.ID, user.ID)
	}
	if req.Test && !agent.AllowTest && !caller.Simulated {
		return CreateResult{}, fmt.Errorf("%w: %s may not place test calls", ErrForbidden, agent.ID)
	}

	now := s.now()
	at := now
	switch {
	case !req.At.IsZero():
		if req.At.Before(now.Add(-time.Minute)) {
			return CreateResult{}, invalid("schedule time is in the past")
		}
		if req.At.After(now) {
			at = req.At.UTC()
		}
	case req.After > 0:
		at = now.Add(req.After)
	}
	if at.Sub(now) > s.cfg.MaxScheduleAhead.D() {
		return CreateResult{}, invalid("schedule time is more than %s ahead", s.cfg.MaxScheduleAhead.D())
	}
	expiresIn := req.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = s.cfg.DefaultExpiry.D()
	}

	// Replays of the same request return the call already made.
	if req.IdempotencyKey != "" {
		if prev, err := s.store.CallByIdempotencyKey(ctx, agent.ID, req.IdempotencyKey); err == nil {
			s.metrics.VoiceDuplicates.Inc()
			return CreateResult{Call: prev, Deduplicated: true}, nil
		}
	}
	if prev, err := s.store.FindPendingDuplicate(ctx, agent.ID, user.ID, req.Message); err == nil {
		s.metrics.VoiceDuplicates.Inc()
		return CreateResult{Call: prev, Deduplicated: true}, nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return CreateResult{}, err
	}
	if caller.Origin != OriginRoute {
		pending, err := s.store.CountPending(ctx, agent.ID)
		if err != nil {
			return CreateResult{}, err
		}
		if pending >= agent.MaxPending {
			s.metrics.VoiceSchedulingFailure.WithLabelValues("too_many_pending").Inc()
			return CreateResult{}, &RateLimitedError{Reason: fmt.Sprintf("%d calls already scheduled", pending), RetryAfter: time.Minute}
		}
	}

	interactive := true
	if req.Interactive != nil {
		interactive = *req.Interactive
	}
	c := models.Call{
		ID: newID(), Agent: agent.ID, CallerName: agent.DisplayName, Simulated: caller.Simulated, Test: req.Test,
		Recipient: user.ID, Type: req.Type, Severity: req.Severity, Title: strings.TrimSpace(req.Title),
		Message: req.Message, Details: strings.TrimSpace(req.Details), Interactive: interactive,
		UserRequested: req.UserRequested, IdempotencyKey: req.IdempotencyKey, State: models.CallScheduled,
		Attempt: 1, Origin: orDefault(caller.Origin, OriginAPI), ScheduledAt: at, ExpiresAt: at.Add(expiresIn),
		EventRef: caller.EventRef, LastActivityAt: now, CreatedAt: now, UpdatedAt: now, Incident: req.Incident,
	}
	if c.Test {
		c.CallerName += " (test)"
	} else if c.Simulated {
		c.CallerName += " (simulated)"
	}

	res := CreateResult{Created: true}
	// An immediate request learns now if policy holds it back: rate limits are
	// refused (the agent decides whether to schedule it later), quiet hours
	// defer it to their end (the recipient's rule, not the agent's choice).
	if !at.After(now) && caller.Origin != OriginRoute {
		d, err := s.limits(ctx, c, user, agent, now, false)
		if err != nil {
			return CreateResult{}, err
		}
		switch d.reason {
		case "rate_limit", "cooldown":
			s.metrics.VoiceSchedulingFailure.WithLabelValues(d.reason).Inc()
			return CreateResult{}, &RateLimitedError{Reason: d.reason, RetryAfter: d.deferUntil.Sub(now)}
		case "quiet_hours":
			c.ScheduledAt, c.Reason, res.Deferred = d.deferUntil, "quiet_hours", "quiet_hours"
			c.ExpiresAt = c.ScheduledAt.Add(expiresIn)
		}
	}

	if c.EventRef == 0 {
		ref, err := s.recordEvent(ctx, c)
		if err != nil {
			return CreateResult{}, err
		}
		c.EventRef = ref
	}
	actor := "agent:" + agent.ID
	if caller.Simulated {
		actor = "admin as " + agent.ID
	}
	stored, err := s.store.InsertCall(ctx, c, actor, describeCreate(c))
	if errors.Is(err, storage.ErrConflict) {
		s.metrics.VoiceDuplicates.Inc()
		return CreateResult{Call: stored, Deduplicated: true}, nil
	}
	if err != nil {
		s.metrics.VoiceSchedulingFailure.WithLabelValues("storage").Inc()
		return CreateResult{}, err
	}
	s.metrics.VoiceCalls.WithLabelValues(c.Origin).Inc()
	if c.ScheduledAt.After(now) {
		s.metrics.VoiceScheduled.WithLabelValues("scheduled").Inc()
	}
	s.log.Info("voice call created", "call", c.ID, "agent", c.Agent, "recipient", c.Recipient, "origin", c.Origin,
		"scheduled_at", c.ScheduledAt, "test", c.Test, "simulated", c.Simulated)
	s.Nudge()
	res.Call = stored
	return res, nil
}

func describeCreate(c models.Call) string {
	d := fmt.Sprintf("%s %s for %s at %s", c.Origin, c.Type, c.Recipient, c.ScheduledAt.Format(time.RFC3339))
	if c.Reason != "" {
		d += " (" + c.Reason + ")"
	}
	return d
}

// recordEvent stores the notification as an ordinary event (audit trail and
// admin UI). It is not routed: the call is its delivery.
func (s *Service) recordEvent(ctx context.Context, c models.Call) (int64, error) {
	ev := models.Event{
		EventID: c.ID, Type: c.Type, Source: "agent:" + c.Agent, Severity: c.Severity,
		Title: orDefault(c.Title, "Voice call from "+c.CallerName), Summary: c.Message, Description: c.Details,
		Labels:    map[string]string{"channel": "voice_call", "agent": c.Agent, "recipient": c.Recipient},
		Timestamp: c.CreatedAt,
	}
	if c.Test {
		ev.Labels["test"] = "true"
	}
	ev.Fingerprint = dedupe.Fingerprint(ev)
	raw, _ := json.Marshal(ev)
	stored, err := s.store.InsertEvent(ctx, ev, raw)
	if err != nil {
		return 0, err
	}
	return stored.ID, nil
}

// CreateFromEvent places a call for an event routed to a voice_call provider.
// The delivery id makes it idempotent across delivery retries.
func (s *Service) CreateFromEvent(ctx context.Context, msg models.NotificationMessage, recipient, agent string) error {
	ev := msg.Event
	text := ev.Summary
	if text == "" {
		text = ev.Title
	}
	req := CreateRequest{
		Recipient: recipient, Type: ev.Type, Severity: ev.Severity, Title: ev.Title, Message: text,
		Details: ev.Description, Test: msg.Test, Incident: incidentFromEvent(ev),
	}
	if msg.DeliveryID != 0 {
		req.IdempotencyKey = fmt.Sprintf("delivery:%d", msg.DeliveryID)
	}
	// A provider test (POST /api/v1/test) is an admin action: the call is
	// marked simulated as well as test.
	_, err := s.CreateCall(ctx, Caller{Agent: agent, Origin: OriginRoute, EventRef: msg.EventRef, Simulated: msg.Test}, req)
	return err
}

// Cancel cancels a scheduled or ringing call. agent "" means an admin.
func (s *Service) Cancel(ctx context.Context, agent, id string) (models.Call, error) {
	c, err := s.store.GetCall(ctx, id)
	if errors.Is(err, storage.ErrNotFound) || (err == nil && agent != "" && c.Agent != agent) {
		return models.Call{}, ErrNotFound
	}
	if err != nil {
		return models.Call{}, err
	}
	actor := "admin"
	if agent != "" {
		actor = "agent:" + agent
	}
	now := s.now()
	reason := "cancelled by " + actor
	c, err = s.store.TransitionCall(ctx, id, storage.CallUpdate{To: models.CallCancelled, Reason: &reason, EndedAt: &now, Actor: actor})
	var te *models.TransitionError
	if errors.As(err, &te) || errors.Is(err, storage.ErrConflict) {
		return c, ErrState
	}
	if err != nil {
		return models.Call{}, err
	}
	s.ended(ctx, c)
	return c, nil
}

// GetForAgent returns a call and its log if agent ("" = admin) may see it.
func (s *Service) GetForAgent(ctx context.Context, agent, id string) (models.Call, []models.CallLogEntry, error) {
	c, err := s.store.GetCall(ctx, id)
	if errors.Is(err, storage.ErrNotFound) || (err == nil && agent != "" && c.Agent != agent) {
		return models.Call{}, nil, ErrNotFound
	}
	if err != nil {
		return models.Call{}, nil, err
	}
	log, err := s.store.CallLog(ctx, id)
	return c, log, err
}

// ---- scheduler and sweeper ----

// Start runs the dispatcher until ctx is cancelled.
func (s *Service) Start(ctx context.Context) {
	s.doneCh = make(chan struct{})
	go func() {
		defer close(s.doneCh)
		t := time.NewTicker(s.poll)
		defer t.Stop()
		for {
			s.Tick(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-s.wake:
			}
		}
	}()
}

// Stop waits for the dispatcher to exit after its context is cancelled.
func (s *Service) Stop() {
	if s.doneCh != nil {
		<-s.doneCh
	}
}

// Nudge wakes the dispatcher without blocking.
func (s *Service) Nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Tick runs one sweep and one dispatch pass. Everything it acts on is read
// from the database, so a restart resumes exactly where it stopped.
func (s *Service) Tick(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	s.sweep(ctx)
	due, err := s.store.DueCalls(ctx, s.now(), 32)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("voice: due calls", "err", err)
		}
		return
	}
	for _, c := range due {
		if err := s.dispatch(ctx, c); err != nil && ctx.Err() == nil {
			s.log.Error("voice: dispatch", "call", c.ID, "err", err)
		}
	}
}

func (s *Service) sweep(ctx context.Context) {
	now := s.now()
	ringing, err := s.store.RingingPastDeadline(ctx, now)
	if err != nil {
		s.log.Error("voice: sweep ringing", "err", err)
	}
	for _, c := range ringing {
		reason := "no answer"
		s.finish(ctx, c.ID, models.CallMissed, reason, "sweeper")
	}
	stale, err := s.store.StaleInProgress(ctx, now.Add(-s.cfg.ActiveTimeout.D()))
	if err != nil {
		s.log.Error("voice: sweep stale", "err", err)
	}
	for _, c := range stale {
		s.finish(ctx, c.ID, models.CallFailed, "no activity from the device", "sweeper")
	}
}

// finish moves a call to a final state from the server side.
func (s *Service) finish(ctx context.Context, id string, to models.CallState, reason, actor string) {
	now := s.now()
	c, err := s.store.TransitionCall(ctx, id, storage.CallUpdate{To: to, Reason: &reason, EndedAt: &now, Actor: actor})
	if err != nil {
		s.log.Warn("voice: finish", "call", id, "to", to, "err", err)
		return
	}
	s.ended(ctx, c)
}

func (s *Service) dispatch(ctx context.Context, c models.Call) error {
	user, okU := s.cfg.User(c.Recipient)
	agent, okA := s.cfg.Agent(c.Agent)
	if !okU || !okA || !contains(user.AllowedAgents, agent.ID) {
		// Config changed since the call was made: the permission is gone.
		s.finish(ctx, c.ID, models.CallFailed, "caller no longer allowed to call recipient", "dispatcher")
		return nil
	}
	now := s.now()
	d, err := s.evaluate(ctx, c, user, agent, now)
	if err != nil {
		return err
	}
	if d.expire {
		reason := "expired before it could ring"
		if d.reason != "expired" {
			reason += " (" + d.reason + ")"
		}
		s.finishScheduled(ctx, c, models.CallExpired, reason)
		return nil
	}
	if !d.ring {
		s.metrics.VoiceDeferred.WithLabelValues(d.reason).Inc()
		return s.store.DeferCall(ctx, c.ID, d.deferUntil, d.expiresAt, "dispatcher", d.reason)
	}
	devices, err := s.store.ListDevices(ctx, user.ID, true)
	if err != nil {
		return err
	}
	if len(devices) == 0 {
		s.finishScheduled(ctx, c, models.CallFailed, "no enrolled device")
		return nil
	}
	return s.ring(ctx, c, devices, now)
}

func (s *Service) finishScheduled(ctx context.Context, c models.Call, to models.CallState, reason string) {
	s.finish(ctx, c.ID, to, reason, "dispatcher")
}

// ring claims the call (scheduled → ringing, conditional, so it rings once)
// and then sends an invitation to every device.
func (s *Service) ring(ctx context.Context, c models.Call, devices []models.Device, now time.Time) error {
	deadline := now.Add(s.cfg.RingTimeout.D())
	reason := ""
	c, err := s.store.TransitionCall(ctx, c.ID, storage.CallUpdate{To: models.CallRinging, RangAt: &now,
		RingDeadline: &deadline, Reason: &reason, Actor: "dispatcher", Detail: fmt.Sprintf("%d device(s)", len(devices))})
	if err != nil {
		// Cancelled or claimed in the meantime: nothing to ring.
		return nil
	}
	inv := Invitation{CallID: c.ID, CallerName: c.CallerName, CallerID: c.Agent, Test: c.Test, Simulated: c.Simulated,
		ExpiresAt: deadline, RingSeconds: int(s.cfg.DeviceRingTimeout.D() / time.Second)}

	sent, failed := 0, 0
	for _, dev := range devices {
		status, errMsg := s.invite(ctx, dev, inv)
		switch status {
		case "sent", "poll":
			sent++
		default:
			failed++
		}
		if err := s.store.RecordRing(ctx, c.ID, dev.ID, status, errMsg); err != nil {
			s.log.Error("voice: record ring", "call", c.ID, "device", dev.ID, "err", err)
		}
	}
	pushDone := s.now()
	due := c.ScheduledAt
	if c.CreatedAt.After(due) {
		due = c.CreatedAt
	}
	s.metrics.VoiceCallSetup.Observe(pushDone.Sub(due).Seconds())
	s.log.Info("voice call ringing", "call", c.ID, "devices", len(devices), "invited", sent, "failed", failed,
		"push_ms", pushDone.Sub(now).Milliseconds(), "push", s.pusher != nil)
	if sent == 0 {
		s.finish(ctx, c.ID, models.CallFailed, "no device could be rung", "dispatcher")
	}
	return nil
}

// invite rings one device. Without APNs, or for a device that has no VoIP
// token, the invitation waits for the device to poll (development only).
func (s *Service) invite(ctx context.Context, dev models.Device, inv Invitation) (string, string) {
	if s.pusher == nil || !dev.HasVoIPToken {
		s.metrics.VoicePushes.WithLabelValues("poll").Inc()
		return "poll", ""
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := s.pusher.Push(pctx, dev, inv)
	switch {
	case err == nil:
		s.metrics.VoicePushes.WithLabelValues("sent").Inc()
		return "sent", ""
	case errors.Is(err, ErrBadDeviceToken):
		s.metrics.VoicePushes.WithLabelValues("bad_token").Inc()
		s.log.Warn("voice: device token rejected; cleared", "device", dev.ID, "err", err)
		_ = s.store.UpdateDevicePush(ctx, dev.ID, "", dev.APNSEnvironment)
		return "bad_token", err.Error()
	default:
		s.metrics.VoicePushes.WithLabelValues("failed").Inc()
		s.log.Warn("voice: push failed", "device", dev.ID, "err", err)
		return "failed", err.Error()
	}
}

// ended runs once a call reaches a final state: metrics, a retry after a
// missed call, and the fallback notification when the call went unanswered.
func (s *Service) ended(ctx context.Context, c models.Call) {
	s.metrics.VoiceCallsEnded.WithLabelValues(string(c.State), string(c.Outcome)).Inc()
	switch c.State {
	case models.CallDeclined:
		s.metrics.VoiceCallsDeclined.Inc()
	case models.CallMissed:
		s.metrics.VoiceCallsMissed.Inc()
	case models.CallFailed:
		s.metrics.VoiceCallsFailed.Inc()
	}
	if c.AnsweredAt != nil && c.EndedAt != nil {
		s.metrics.VoiceCallDuration.Observe(c.EndedAt.Sub(*c.AnsweredAt).Seconds())
	}
	s.log.Info("voice call ended", "call", c.ID, "state", c.State, "outcome", c.Outcome, "reason", c.Reason)

	user, ok := s.cfg.User(c.Recipient)
	if !ok {
		return
	}
	unanswered := c.State == models.CallMissed || c.State == models.CallExpired ||
		(c.State == models.CallFailed && c.AnsweredAt == nil)
	if c.State == models.CallMissed && c.Attempt < user.Retry.MaxAttempts {
		at := s.now().Add(user.Retry.Delay.D())
		if at.Before(c.ExpiresAt) {
			if err := s.followUp(ctx, c, OriginRetry, at, c.Attempt+1, "retry"); err == nil {
				return
			}
		}
	}
	if unanswered && user.FallbackProvider != "" && s.fallback != nil && c.EventRef != 0 && !c.Test {
		if err := s.fallback.SendFallback(ctx, c.EventRef, user.FallbackProvider); err != nil {
			s.log.Error("voice: fallback", "call", c.ID, "provider", user.FallbackProvider, "err", err)
			return
		}
		_ = s.store.AppendCallLog(ctx, models.CallLogEntry{CallID: c.ID, Kind: "fallback", Actor: "server",
			Detail: "sent through provider " + user.FallbackProvider})
	}
}

// followUp schedules the next call for the same notification.
func (s *Service) followUp(ctx context.Context, prev models.Call, origin string, at time.Time, attempt int, reason string) error {
	now := s.now()
	next := prev
	next.ID, next.State, next.Outcome, next.Reason = newID(), models.CallScheduled, models.OutcomeNone, reason
	next.Origin, next.Attempt, next.ParentCallID = origin, attempt, prev.ID
	next.IdempotencyKey = origin + ":" + prev.ID
	next.ScheduledAt = at
	next.RingDeadline, next.RangAt, next.AnsweredAt, next.EndedAt, next.AnsweredBy = nil, nil, nil, nil, ""
	next.LastActivityAt, next.CreatedAt, next.UpdatedAt = now, now, now
	if _, err := s.store.InsertCall(ctx, next, "server", reason+" of "+prev.ID); err != nil {
		if !errors.Is(err, storage.ErrConflict) {
			s.metrics.VoiceSchedulingFailure.WithLabelValues("storage").Inc()
			s.log.Error("voice: follow-up", "call", prev.ID, "err", err)
		}
		return err
	}
	s.metrics.VoiceCalls.WithLabelValues(origin).Inc()
	s.metrics.VoiceScheduled.WithLabelValues(reason).Inc()
	s.Nudge()
	return nil
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
