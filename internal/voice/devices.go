package voice

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pod32g/omni-notify/internal/config"
	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/storage"
)

// DeviceTokenPrefix marks device credentials so the API can tell them apart
// from admin and agent tokens without a database lookup for every request.
const DeviceTokenPrefix = "ond_"

// HashSecret is how enrollment codes and device secrets are stored.
func HashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

var codeEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// CreateEnrollment mints a one-time code that enrolls a phone for user.
func (s *Service) CreateEnrollment(ctx context.Context, user string) (string, time.Time, error) {
	u, ok := s.cfg.User(user)
	if !ok {
		return "", time.Time{}, invalid("unknown user %q", user)
	}
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	raw := codeEncoding.EncodeToString(b) // 16 characters
	code := raw[:4] + "-" + raw[4:8] + "-" + raw[8:12] + "-" + raw[12:16]
	expires := s.now().Add(s.cfg.EnrollmentTTL.D())
	if err := s.store.CreateEnrollment(ctx, HashSecret(normalizeCode(code)), u.ID, expires); err != nil {
		return "", time.Time{}, err
	}
	return code, expires, nil
}

func normalizeCode(c string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(c))
}

// EnrollRequest describes the phone being enrolled.
type EnrollRequest struct {
	Code            string `json:"code"`
	Name            string `json:"name"`
	Platform        string `json:"platform"`
	BundleID        string `json:"bundle_id"`
	APNSEnvironment string `json:"apns_environment"`
	VoIPToken       string `json:"voip_token"`
}

// Enroll redeems a code and returns the new device and its secret, which is
// shown once and stored only as a hash.
func (s *Service) Enroll(ctx context.Context, r EnrollRequest) (models.Device, string, error) {
	if r.Code == "" {
		return models.Device{}, "", invalid("code is required")
	}
	env, err := apnsEnv(r.APNSEnvironment)
	if err != nil {
		return models.Device{}, "", err
	}
	if err := checkToken(r.VoIPToken); err != nil {
		return models.Device{}, "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return models.Device{}, "", err
	}
	secret := DeviceTokenPrefix + hex.EncodeToString(b)
	d := models.Device{
		ID: newID(), Name: truncate(orDefault(strings.TrimSpace(r.Name), "iPhone"), 80),
		Platform: orDefault(r.Platform, "ios"), BundleID: truncate(r.BundleID, 200), APNSEnvironment: env,
		VoIPToken: r.VoIPToken, SecretHash: HashSecret(secret),
	}
	d, err = s.store.RedeemEnrollment(ctx, HashSecret(normalizeCode(r.Code)), d)
	if errors.Is(err, storage.ErrNotFound) {
		return models.Device{}, "", fmt.Errorf("%w: enrollment code is unknown, used or expired", ErrForbidden)
	}
	if err != nil {
		return models.Device{}, "", err
	}
	s.log.Info("voice device enrolled", "device", d.ID, "user", d.User, "name", d.Name, "env", d.APNSEnvironment)
	return d, secret, nil
}

func apnsEnv(e string) (string, error) {
	switch e {
	case "", "development", "sandbox":
		return "development", nil
	case "production":
		return "production", nil
	}
	return "", invalid("apns_environment must be development or production")
}

func checkToken(t string) error {
	if len(t) > 200 || strings.Trim(strings.ToLower(t), "0123456789abcdef") != "" {
		return invalid("voip_token must be the hex device token")
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// AuthenticateDevice resolves a device credential.
func (s *Service) AuthenticateDevice(ctx context.Context, secret string) (models.Device, error) {
	d, err := s.store.DeviceBySecretHash(ctx, HashSecret(secret))
	if err != nil {
		return models.Device{}, err
	}
	_ = s.store.TouchDevice(ctx, d.ID)
	return d, nil
}

// UpdatePush stores a device's current VoIP token.
func (s *Service) UpdatePush(ctx context.Context, d models.Device, token, env string) error {
	e, err := apnsEnv(env)
	if err != nil {
		return err
	}
	if err := checkToken(token); err != nil {
		return err
	}
	return s.store.UpdateDevicePush(ctx, d.ID, token, e)
}

// Ringing lists the invitations waiting for a device: the poll transport
// that stands in for VoIP push in development, and how a second device
// learns a call it is showing was answered elsewhere.
func (s *Service) Ringing(ctx context.Context, d models.Device) ([]Invitation, error) {
	calls, err := s.store.RingingForDevice(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	out := make([]Invitation, 0, len(calls))
	for _, c := range calls {
		inv := Invitation{CallID: c.ID, CallerName: c.CallerName, CallerID: c.Agent, Test: c.Test,
			Simulated: c.Simulated, RingSeconds: int(s.cfg.DeviceRingTimeout.D() / time.Second)}
		if c.RingDeadline != nil {
			inv.ExpiresAt = *c.RingDeadline
		}
		out = append(out, inv)
	}
	return out, nil
}

// DeviceCall is the phone's view of a call.
type DeviceCall struct {
	ID          string           `json:"id"`
	State       models.CallState `json:"state"`
	CallerName  string           `json:"caller_name"`
	CallerID    string           `json:"caller_id"`
	Title       string           `json:"title,omitempty"`
	Test        bool             `json:"test"`
	Simulated   bool             `json:"simulated"`
	Interactive bool             `json:"interactive"`
	Greeting    Utterance        `json:"greeting"`
	// GreetingPauseMs is the longest pause after the greeting (GreetingPause).
	GreetingPauseMs int        `json:"greeting_pause_ms"`
	Opening         Utterance  `json:"opening"`
	Phrases         []string   `json:"phrases"`
	RingDeadline    *time.Time `json:"ring_deadline,omitempty"`
	RingSeconds     int        `json:"ring_seconds"`
	AnsweredHere    bool       `json:"answered_here"`
}

func (s *Service) deviceView(c models.Call, d models.Device) DeviceCall {
	phrases := []string{}
	if p, ok := s.responder.(Phraser); ok && c.Interactive {
		phrases = p.Phrases(c)
	}
	return DeviceCall{
		Phrases: phrases,
		ID:      c.ID, State: c.State, CallerName: c.CallerName, CallerID: c.Agent, Title: c.Title, Test: c.Test,
		Simulated: c.Simulated, Interactive: c.Interactive, Greeting: s.responder.Greeting(c),
		GreetingPauseMs: int(GreetingPause / time.Millisecond), Opening: s.responder.Opening(c),
		RingDeadline: c.RingDeadline, RingSeconds: int(s.cfg.DeviceRingTimeout.D() / time.Second),
		AnsweredHere: c.AnsweredBy == d.ID,
	}
}

// deviceCall loads a call the device was rung for.
func (s *Service) deviceCall(ctx context.Context, d models.Device, id string) (models.Call, error) {
	rung, err := s.store.DeviceWasRung(ctx, id, d.ID)
	if err != nil {
		return models.Call{}, err
	}
	if !rung {
		return models.Call{}, ErrNotFound
	}
	c, err := s.store.GetCall(ctx, id)
	if errors.Is(err, storage.ErrNotFound) {
		return models.Call{}, ErrNotFound
	}
	return c, err
}

// DeviceGet returns the device's view of a call it was rung for.
func (s *Service) DeviceGet(ctx context.Context, d models.Device, id string) (DeviceCall, error) {
	c, err := s.deviceCall(ctx, d, id)
	if err != nil {
		return DeviceCall{}, err
	}
	return s.deviceView(c, d), nil
}

// Report is what a phone says happened to a call, with its own timings.
type Report struct {
	State        string `json:"state"`
	Reason       string `json:"reason"`
	FirstAudioMs int    `json:"first_audio_ms"`
	TTSMs        int    `json:"tts_ms"`
}

// DeviceReport applies a state change reported by a phone. Repeating a
// report already applied is harmless; one that the call's state no longer
// allows returns ErrState with the current view (for example "answered on
// another device").
func (s *Service) DeviceReport(ctx context.Context, d models.Device, id string, r Report) (DeviceCall, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.deviceCall(ctx, d, id)
	if err != nil {
		return DeviceCall{}, err
	}
	to := models.CallState(r.State)
	switch to {
	case models.CallAnswered, models.CallActive, models.CallDeclined, models.CallMissed, models.CallCompleted, models.CallFailed:
	default:
		return DeviceCall{}, invalid("state must be answered, active, declined, missed, completed or failed")
	}
	// After an answer, only the answering phone speaks for the call.
	if c.AnsweredBy != "" && c.AnsweredBy != d.ID {
		return s.deviceView(c, d), ErrState
	}
	if c.State == to {
		return s.deviceView(c, d), nil
	}
	now := s.now()
	u := storage.CallUpdate{To: to, Actor: "device:" + d.ID, Detail: truncate(r.Reason, 300)}
	if r.Reason != "" {
		reason := truncate(r.Reason, 300)
		u.Reason = &reason
	}
	switch to {
	case models.CallAnswered:
		u.AnsweredAt, u.AnsweredBy = &now, &d.ID
	case models.CallActive:
	default:
		u.EndedAt = &now
	}
	c, err = s.store.TransitionCall(ctx, id, u)
	var te *models.TransitionError
	if errors.As(err, &te) || errors.Is(err, storage.ErrConflict) {
		cur, gerr := s.store.GetCall(ctx, id)
		if gerr != nil {
			return DeviceCall{}, gerr
		}
		return s.deviceView(cur, d), ErrState
	}
	if err != nil {
		return DeviceCall{}, err
	}
	switch {
	case to == models.CallAnswered:
		s.metrics.VoiceCallsAnswered.Inc()
		if c.RangAt != nil {
			s.metrics.VoiceTimeToAnswer.Observe(now.Sub(*c.RangAt).Seconds())
		}
	case to == models.CallActive:
		if r.FirstAudioMs > 0 {
			s.metrics.VoiceFirstAudio.Observe(float64(r.FirstAudioMs) / 1000)
		}
	case to.Terminal():
		s.ended(ctx, c)
	}
	if r.TTSMs > 0 {
		s.metrics.VoiceTTSLatency.Observe(float64(r.TTSMs) / 1000)
	}
	return s.deviceView(c, d), nil
}

// TurnRequest is one thing the recipient said, transcribed on the phone.
type TurnRequest struct {
	Seq        int    `json:"seq"`
	Transcript string `json:"transcript"`
	STTMs      int    `json:"stt_ms"`
	TTSMs      int    `json:"tts_ms"`
}

// ActionReport says what was done with a turn's action.
type ActionReport struct {
	Kind   string     `json:"kind"`
	Status string     `json:"status"` // persisted | refused
	CallID string     `json:"call_id,omitempty"`
	At     *time.Time `json:"at,omitempty"`
}

// TurnResponse is what the phone says back, and whether to hang up after.
type TurnResponse struct {
	Seq     int           `json:"seq"`
	Intent  IntentKind    `json:"intent"`
	Reply   Utterance     `json:"reply"`
	EndCall bool          `json:"end_call"`
	Action  *ActionReport `json:"action,omitempty"`
}

// Turn interprets one utterance, applies its action and returns the reply.
// The action and the reply are committed together before anything is sent
// back, so the phone never confirms something that was not stored; a retried
// request with the same seq replays the stored reply.
func (s *Service) Turn(ctx context.Context, d models.Device, id string, r TurnRequest) (TurnResponse, error) {
	start := time.Now()
	c, err := s.deviceCall(ctx, d, id)
	if err != nil {
		return TurnResponse{}, err
	}
	if r.Seq < 1 || r.Seq > 1000 {
		return TurnResponse{}, invalid("seq must be between 1 and 1000")
	}
	if len(r.Transcript) > 2000 {
		return TurnResponse{}, invalid("transcript is too long")
	}
	if prev, err := s.store.GetTurn(ctx, id, r.Seq); err == nil {
		return decodeTurn(prev)
	}
	if c.AnsweredBy != d.ID || (c.State != models.CallAnswered && c.State != models.CallActive) || !c.Interactive {
		return TurnResponse{}, ErrState
	}
	user, okU := s.cfg.User(c.Recipient)
	agent, okA := s.cfg.Agent(c.Agent)
	if !okU || !okA {
		return TurnResponse{}, ErrState
	}
	loc := user.Location()
	now := s.now()
	in, err := s.responder.Interpret(ctx, c, r.Transcript, now, loc)
	if err != nil {
		return TurnResponse{}, err
	}
	misses, err := s.store.CountTurns(ctx, id, string(IntentUnknown), string(IntentSilence))
	if err != nil {
		return TurnResponse{}, err
	}
	res, fx, report := s.apply(c, user, agent, in, r.Seq, now, d)
	utter, end := s.responder.Reply(c, in, res, misses, now, loc)
	resp := TurnResponse{Seq: r.Seq, Intent: in.Kind, Reply: utter, EndCall: end, Action: report}
	body, err := json.Marshal(resp)
	if err != nil {
		return TurnResponse{}, err
	}
	t := storage.Turn{CallID: id, Seq: r.Seq, DeviceID: d.ID, Intent: string(in.Kind), Response: string(body)}
	if s.cfg.StoreTranscripts {
		t.Transcript = r.Transcript
	}
	stored, err := s.store.RecordTurn(ctx, t, fx)
	if errors.Is(err, storage.ErrConflict) {
		return decodeTurn(stored)
	}
	if err != nil {
		if fx.FollowUp != nil {
			s.metrics.VoiceSchedulingFailure.WithLabelValues("storage").Inc()
		}
		return TurnResponse{}, err
	}
	s.metrics.VoiceTurns.WithLabelValues(string(in.Kind)).Inc()
	s.metrics.VoiceTurnLatency.Observe(time.Since(start).Seconds())
	if r.STTMs > 0 {
		s.metrics.VoiceSTTLatency.Observe(float64(r.STTMs) / 1000)
	}
	if r.TTSMs > 0 {
		s.metrics.VoiceTTSLatency.Observe(float64(r.TTSMs) / 1000)
	}
	if fx.FollowUp != nil {
		s.metrics.VoiceCalls.WithLabelValues(OriginUser).Inc()
		s.metrics.VoiceScheduled.WithLabelValues(fx.FollowUp.Reason).Inc()
		s.log.Info("voice follow-up scheduled", "call", id, "follow_up", fx.FollowUp.ID, "at", fx.FollowUp.ScheduledAt,
			"reason", fx.FollowUp.Reason)
		s.Nudge()
	}
	return resp, nil
}

func decodeTurn(t storage.Turn) (TurnResponse, error) {
	var r TurnResponse
	err := json.Unmarshal([]byte(t.Response), &r)
	return r, err
}

// apply validates an intent's action against the caller's grants and the
// recipient's policy and describes its persistent effects.
func (s *Service) apply(c models.Call, u config.VoiceUser, a config.VoiceAgent, in Intent, seq int, now time.Time, d models.Device) (ActionResult, storage.TurnEffects, *ActionReport) {
	var fx storage.TurnEffects
	kind := string(in.Kind)
	refuse := func(why string) (ActionResult, storage.TurnEffects, *ActionReport) {
		return ActionResult{Refused: why}, fx, &ActionReport{Kind: kind, Status: "refused"}
	}
	logAction := func(detail string) {
		fx.Log = append(fx.Log, models.CallLogEntry{CallID: c.ID, Kind: "action", Actor: "device:" + d.ID, Detail: detail})
	}
	switch in.Kind {
	case IntentAcknowledge, IntentDismiss:
		if !contains(a.Actions, kind) {
			// Not an action this caller offers: treat it as a plain goodbye.
			return ActionResult{}, fx, nil
		}
		o := models.OutcomeAcknowledged
		if in.Kind == IntentDismiss {
			o = models.OutcomeDismissed
		}
		fx.Outcome = &o
		logAction(string(o))
		return ActionResult{Done: true}, fx, &ActionReport{Kind: kind, Status: "persisted"}
	case IntentSnooze, IntentReschedule:
		if !contains(a.Actions, kind) {
			return refuse(fmt.Sprintf("I can't %s calls from %s.", kind, c.CallerName))
		}
		var at time.Time
		if in.Kind == IntentSnooze {
			after := in.After
			if after < time.Minute {
				after = time.Minute
			}
			if after > u.MaxSnooze.D() {
				return refuse("I can only snooze for up to " + SayDuration(u.MaxSnooze.D()) + ".")
			}
			at = now.Add(after)
		} else {
			at = in.At
			if !at.After(now.Add(30 * time.Second)) {
				return refuse("That time has already passed.")
			}
			if at.Sub(now) > s.cfg.MaxScheduleAhead.D() {
				return refuse("That's too far ahead for me.")
			}
		}
		res := ActionResult{Done: true, At: at}
		next := c
		next.ID, next.State, next.Outcome, next.Origin = newID(), models.CallScheduled, models.OutcomeNone, OriginUser
		next.Reason, next.Attempt, next.ParentCallID = kind, 1, c.ID
		next.UserRequested, next.IdempotencyKey = true, fmt.Sprintf("turn:%s:%d", c.ID, seq)
		if end, quiet := quietUntil(u, at); quiet && !exempt(next, u, a) {
			at, res.At, res.AfterQuiet = end, end, true
		}
		next.ScheduledAt, next.ExpiresAt = at, at.Add(s.cfg.DefaultExpiry.D())
		next.RingDeadline, next.RangAt, next.AnsweredAt, next.EndedAt, next.AnsweredBy = nil, nil, nil, nil, ""
		next.LastActivityAt, next.CreatedAt, next.UpdatedAt = now, now, now
		fx.FollowUp = &next
		o := models.OutcomeSnoozed
		if in.Kind == IntentReschedule {
			o = models.OutcomeRescheduled
		}
		fx.Outcome = &o
		logAction(fmt.Sprintf("%s: next call %s at %s", o, next.ID, at.Format(time.RFC3339)))
		atCopy := at
		return res, fx, &ActionReport{Kind: kind, Status: "persisted", CallID: next.ID, At: &atCopy}
	}
	return ActionResult{}, fx, nil
}
