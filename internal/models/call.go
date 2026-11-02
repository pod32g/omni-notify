package models

import (
	"fmt"
	"time"
)

// CallState is the lifecycle position of one voice call (one ringing attempt).
// The calls table doubles as the voice scheduler: a SCHEDULED row whose
// scheduled_at has passed is claimed by the transition to RINGING.
type CallState string

const (
	CallScheduled CallState = "scheduled"
	CallRinging   CallState = "ringing"
	CallAnswered  CallState = "answered"
	CallActive    CallState = "active"
	CallCompleted CallState = "completed"
	CallDeclined  CallState = "declined"
	CallMissed    CallState = "missed"
	CallExpired   CallState = "expired"
	CallFailed    CallState = "failed"
	CallCancelled CallState = "cancelled"
)

// callTransitions is the complete set of allowed state changes. Anything not
// listed is rejected, so a late or duplicated device report can never move a
// finished call.
var callTransitions = map[CallState][]CallState{
	CallScheduled: {CallRinging, CallCancelled, CallExpired, CallFailed},
	CallRinging:   {CallAnswered, CallDeclined, CallMissed, CallCancelled, CallFailed},
	CallAnswered:  {CallActive, CallCompleted, CallFailed},
	CallActive:    {CallCompleted, CallFailed},
}

// Valid reports whether s is a known call state.
func (s CallState) Valid() bool {
	switch s {
	case CallScheduled, CallRinging, CallAnswered, CallActive, CallCompleted,
		CallDeclined, CallMissed, CallExpired, CallFailed, CallCancelled:
		return true
	}
	return false
}

// Terminal reports whether no further transition is possible.
func (s CallState) Terminal() bool {
	_, ok := callTransitions[s]
	return s.Valid() && !ok
}

// Live reports whether the call is ringing or in progress (the recipient is busy).
func (s CallState) Live() bool {
	return s == CallRinging || s == CallAnswered || s == CallActive
}

// CanTransition reports whether from → to is an allowed change.
func CanTransition(from, to CallState) bool {
	for _, t := range callTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// CheckTransition returns a descriptive error for a disallowed change.
func CheckTransition(from, to CallState) error {
	if !to.Valid() {
		return fmt.Errorf("unknown call state %q", to)
	}
	if !CanTransition(from, to) {
		return &TransitionError{From: from, To: to}
	}
	return nil
}

// TransitionError reports a rejected state change.
type TransitionError struct {
	From, To CallState
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("call cannot go from %s to %s", e.From, e.To)
}

// CallOutcome is what the recipient decided about the notification, separate
// from whether the call itself connected: answering is not acknowledging.
type CallOutcome string

const (
	OutcomeNone         CallOutcome = ""
	OutcomeAcknowledged CallOutcome = "acknowledged"
	OutcomeDismissed    CallOutcome = "dismissed"
	OutcomeSnoozed      CallOutcome = "snoozed"
	OutcomeRescheduled  CallOutcome = "rescheduled"
)

// Call is one voice call: who placed it, for whom, what to say, and where it is
// in its lifecycle. Retries and snoozes are new calls linked by ParentCallID.
type Call struct {
	ID             string      `json:"id"`
	EventRef       int64       `json:"event_ref"`
	Agent          string      `json:"agent"`
	CallerName     string      `json:"caller_name"`
	Simulated      bool        `json:"simulated"`
	Test           bool        `json:"test"`
	Recipient      string      `json:"recipient"`
	Type           string      `json:"type"`
	Severity       Severity    `json:"severity,omitempty"`
	Title          string      `json:"title"`
	Message        string      `json:"message"`
	Details        string      `json:"details,omitempty"`
	Interactive    bool        `json:"interactive"`
	UserRequested  bool        `json:"user_requested"`
	IdempotencyKey string      `json:"idempotency_key,omitempty"`
	State          CallState   `json:"state"`
	Outcome        CallOutcome `json:"outcome,omitempty"`
	Reason         string      `json:"reason,omitempty"`
	Attempt        int         `json:"attempt"`
	ParentCallID   string      `json:"parent_call_id,omitempty"`
	Origin         string      `json:"origin"`
	ScheduledAt    time.Time   `json:"scheduled_at"`
	ExpiresAt      time.Time   `json:"expires_at"`
	RingDeadline   *time.Time  `json:"ring_deadline,omitempty"`
	RangAt         *time.Time  `json:"rang_at,omitempty"`
	AnsweredAt     *time.Time  `json:"answered_at,omitempty"`
	AnsweredBy     string      `json:"answered_by,omitempty"`
	EndedAt        *time.Time  `json:"ended_at,omitempty"`
	LastActivityAt time.Time   `json:"last_activity_at"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
}

// CallLogEntry is one audited step of a call: a state change, a deferral, a
// push result, or an action taken during the call.
type CallLogEntry struct {
	ID     int64     `json:"id"`
	CallID string    `json:"call_id"`
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	From   CallState `json:"from,omitempty"`
	To     CallState `json:"to,omitempty"`
	Actor  string    `json:"actor"`
	Detail string    `json:"detail,omitempty"`
}

// Device is a phone enrolled to receive voice calls for a user.
type Device struct {
	ID              string     `json:"id"`
	User            string     `json:"user"`
	Name            string     `json:"name"`
	Platform        string     `json:"platform"`
	BundleID        string     `json:"bundle_id"`
	APNSEnvironment string     `json:"apns_environment"`
	VoIPToken       string     `json:"-"`
	HasVoIPToken    bool       `json:"has_voip_token"`
	SecretHash      string     `json:"-"`
	Disabled        bool       `json:"disabled"`
	CreatedAt       time.Time  `json:"created_at"`
	LastSeenAt      *time.Time `json:"last_seen_at,omitempty"`
}
