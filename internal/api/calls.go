package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/storage"
	"github.com/pod32g/omni-notify/internal/voice"
)

// createCallRequest is the body of POST /api/v1/calls. Field names follow the
// event API (snake_case, severity rather than priority). The caller's agent
// identity comes from its credential: "agent" is only a cross-check for
// agents, and for an admin it names who the call is placed for, which marks
// the call simulated.
type createCallRequest struct {
	Recipient       string          `json:"recipient"`
	Agent           string          `json:"agent"`
	DeliveryChannel string          `json:"delivery_channel"`
	Type            string          `json:"type"`
	Severity        models.Severity `json:"severity"`
	Title           string          `json:"title"`
	Message         string          `json:"message"`
	Details         string          `json:"details"`
	Schedule        *struct {
		At *time.Time `json:"at"`
		In string     `json:"in"`
	} `json:"schedule"`
	ExpiresIn      string `json:"expires_in"`
	Interactive    *bool  `json:"interactive"`
	UserRequested  bool   `json:"user_requested"`
	IdempotencyKey string `json:"idempotency_key"`
	Test           bool   `json:"test"`
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// writeVoiceError maps voice service errors to status codes.
func (s *Server) writeVoiceError(w http.ResponseWriter, err error) {
	var rl *voice.RateLimitedError
	switch {
	case errors.As(err, &rl):
		w.Header().Set("Retry-After", strconv.Itoa(int(rl.RetryAfter.Seconds()+0.999)))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": rl.Error(), "reason": rl.Reason, "retry_after_seconds": int(rl.RetryAfter.Seconds() + 0.999),
		})
	case errors.Is(err, voice.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, voice.ErrForbidden):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, voice.ErrNotFound), errors.Is(err, storage.ErrNotFound):
		writeError(w, http.StatusNotFound, "call not found")
	case errors.Is(err, voice.ErrState):
		writeError(w, http.StatusConflict, err.Error())
	default:
		s.log.Error("voice", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func (s *Server) handleCreateCall(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.kind != principalAdmin && p.kind != principalAgent {
		writeError(w, http.StatusForbidden, "only agents and admins place calls")
		return
	}
	var req createCallRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if ch := strings.ToLower(req.DeliveryChannel); ch != "" && ch != "voice_call" {
		writeError(w, http.StatusBadRequest, "delivery_channel must be voice_call on this endpoint")
		return
	}
	caller := voice.Caller{Origin: voice.OriginAPI}
	switch p.kind {
	case principalAgent:
		if req.Agent != "" && req.Agent != p.agent {
			writeError(w, http.StatusForbidden, "agent does not match the authenticated agent")
			return
		}
		caller.Agent = p.agent
	case principalAdmin:
		if req.Agent == "" {
			writeError(w, http.StatusBadRequest, "agent is required: an admin places a call for an agent, and it is marked simulated")
			return
		}
		caller.Agent, caller.Simulated = req.Agent, true
	}
	cr := voice.CreateRequest{
		Recipient: req.Recipient, Type: req.Type, Severity: req.Severity, Title: req.Title,
		Message: req.Message, Details: req.Details, Interactive: req.Interactive,
		UserRequested: req.UserRequested, Test: req.Test,
		IdempotencyKey: firstNonEmpty(req.IdempotencyKey, r.Header.Get("Idempotency-Key")),
	}
	if req.Schedule != nil {
		if req.Schedule.At != nil {
			cr.At = *req.Schedule.At
		}
		if req.Schedule.In != "" {
			d, err := time.ParseDuration(req.Schedule.In)
			if err != nil || d <= 0 {
				writeError(w, http.StatusBadRequest, "schedule.in must be a positive duration like 10m")
				return
			}
			if !cr.At.IsZero() {
				writeError(w, http.StatusBadRequest, "use schedule.at or schedule.in, not both")
				return
			}
			cr.After = d
		}
	}
	if req.ExpiresIn != "" {
		d, err := time.ParseDuration(req.ExpiresIn)
		if err != nil || d <= 0 {
			writeError(w, http.StatusBadRequest, "expires_in must be a positive duration like 2h")
			return
		}
		cr.ExpiresIn = d
	}
	res, err := s.cfg.Voice.CreateCall(r.Context(), caller, cr)
	if err != nil {
		s.writeVoiceError(w, err)
		return
	}
	status := http.StatusCreated
	if res.Deduplicated {
		status = http.StatusOK
	}
	writeJSON(w, status, res)
}

func (s *Server) handleListCalls(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.kind == principalDevice {
		writeError(w, http.StatusForbidden, "devices list their calls at /api/v1/devices/self/ringing")
		return
	}
	q := r.URL.Query()
	f := storage.CallFilter{Recipient: q.Get("recipient"), Limit: atoiDefault(q.Get("limit"), 100)}
	if p.kind == principalAgent {
		f.Agent = p.agent
	} else {
		f.Agent = q.Get("agent")
	}
	if f.Recipient != "" {
		if u, ok := s.cfg.Voice.Config().User(f.Recipient); ok {
			f.Recipient = u.ID
		}
	}
	for _, st := range strings.Split(q.Get("state"), ",") {
		if st = strings.TrimSpace(st); st != "" {
			if !models.CallState(st).Valid() {
				writeError(w, http.StatusBadRequest, "unknown state "+st)
				return
			}
			f.States = append(f.States, models.CallState(st))
		}
	}
	calls, err := s.store.ListCalls(r.Context(), f)
	if err != nil {
		s.writeVoiceError(w, err)
		return
	}
	if calls == nil {
		calls = []models.Call{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"calls": calls, "count": len(calls)})
}

func (s *Server) handleGetCall(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	id := r.PathValue("id")
	if p.kind == principalDevice {
		v, err := s.cfg.Voice.DeviceGet(r.Context(), p.device, id)
		if err != nil {
			s.writeVoiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
		return
	}
	c, log, err := s.cfg.Voice.GetForAgent(r.Context(), p.agent, id)
	if err != nil {
		s.writeVoiceError(w, err)
		return
	}
	if log == nil {
		log = []models.CallLogEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"call": c, "log": log})
}

func (s *Server) handleCancelCall(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.kind == principalDevice {
		writeError(w, http.StatusForbidden, "devices end calls through /state")
		return
	}
	c, err := s.cfg.Voice.Cancel(r.Context(), p.agent, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, voice.ErrState) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "only scheduled or ringing calls can be cancelled", "call": c})
			return
		}
		s.writeVoiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"call": c})
}

func (s *Server) handleCallState(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.kind != principalDevice {
		writeError(w, http.StatusForbidden, "only the phone reports call state")
		return
	}
	var rep voice.Report
	if !decodeJSON(w, r, &rep) {
		return
	}
	v, err := s.cfg.Voice.DeviceReport(r.Context(), p.device, r.PathValue("id"), rep)
	if errors.Is(err, voice.ErrState) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "the call has moved on", "call": v})
		return
	}
	if err != nil {
		s.writeVoiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleCallTurn(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.kind != principalDevice {
		writeError(w, http.StatusForbidden, "only the answering phone takes turns")
		return
	}
	var req voice.TurnRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	resp, err := s.cfg.Voice.Turn(r.Context(), p.device, r.PathValue("id"), req)
	if err != nil {
		s.writeVoiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
