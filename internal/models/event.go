// Package models defines the core domain types shared across Omni-Notify.
//
// These types are deliberately free of storage, transport, or provider
// concerns so that every other package can depend on them without creating
// import cycles.
package models

import (
	"fmt"
	"time"
)

// Status describes the lifecycle position of an event. It is intentionally
// limited to the firing/resolved lifecycle pair; everything that is not a
// lifecycle transition is a stateless event with no status. Severity is a
// separate axis (see Severity).
type Status string

const (
	StatusFiring   Status = "firing"
	StatusResolved Status = "resolved"
	// StatusNone is the zero value used by stateless events (no lifecycle).
	StatusNone Status = ""
)

// IsStateful reports whether the status participates in the firing/resolved
// lifecycle. Only those two statuses are tracked as ongoing state.
func (s Status) IsStateful() bool {
	return s == StatusFiring || s == StatusResolved
}

// Valid reports whether s is a recognised lifecycle status (empty allowed).
func (s Status) Valid() bool {
	switch s {
	case StatusNone, StatusFiring, StatusResolved:
		return true
	default:
		return false
	}
}

// legacySeverityStatuses are values that older clients sent as `status` but that
// are really severities. Event.Normalize migrates them to the severity field.
// All five pre-split severity levels are handled so no legacy producer is
// hard-rejected.
var legacySeverityStatuses = map[Status]Severity{
	"critical": SeverityCritical,
	"error":    SeverityError,
	"warning":  SeverityWarning,
	"info":     SeverityInfo,
	"debug":    SeverityDebug,
}

// Severity is an optional, validated severity level, independent of lifecycle.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityError    Severity = "error"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
	SeverityDebug    Severity = "debug"
	SeverityNone     Severity = ""
)

// Valid reports whether sev is a recognised severity (empty allowed).
func (sev Severity) Valid() bool {
	switch sev {
	case SeverityNone, SeverityCritical, SeverityError, SeverityWarning, SeverityInfo, SeverityDebug:
		return true
	default:
		return false
	}
}

// Event is a generic notification event submitted by a producer. Omni-Notify is
// agnostic about where it came from or what it means.
type Event struct {
	EventID     string            `json:"event_id"`
	Type        string            `json:"type"`
	Source      string            `json:"source"`
	Status      Status            `json:"status,omitempty"`
	Severity    Severity          `json:"severity,omitempty"`
	Title       string            `json:"title"`
	Summary     string            `json:"summary,omitempty"`
	Description string            `json:"description,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Timestamp   time.Time         `json:"timestamp"`
	Fingerprint string            `json:"fingerprint,omitempty"`
}

// Normalize applies backward-compatible migrations so that older payloads keep
// working under the lifecycle/severity split. A legacy status of info/warning/
// error is moved into severity (when severity is empty) and the status cleared,
// since those values are severities, not lifecycle states. Call before Validate.
func (e *Event) Normalize() {
	if sev, ok := legacySeverityStatuses[e.Status]; ok {
		if e.Severity == SeverityNone {
			e.Severity = sev
		}
		e.Status = StatusNone
	}
}

// Validate checks required fields and enum constraints. It does not mutate the
// event; fingerprint derivation happens separately in the dedupe package.
// Callers should Normalize first for backward compatibility.
func (e *Event) Validate() error {
	if e == nil {
		return fmt.Errorf("event is nil")
	}
	switch {
	case e.EventID == "":
		return fmt.Errorf("event_id is required")
	case e.Type == "":
		return fmt.Errorf("type is required")
	case e.Source == "":
		return fmt.Errorf("source is required")
	case e.Title == "":
		return fmt.Errorf("title is required")
	case e.Timestamp.IsZero():
		return fmt.Errorf("timestamp is required")
	}
	if !e.Status.Valid() {
		return fmt.Errorf("invalid status %q", e.Status)
	}
	if !e.Severity.Valid() {
		return fmt.Errorf("invalid severity %q", e.Severity)
	}
	for k := range e.Labels {
		if k == "" {
			return fmt.Errorf("label keys must not be empty")
		}
	}
	for k := range e.Annotations {
		if k == "" {
			return fmt.Errorf("annotation keys must not be empty")
		}
	}
	return nil
}

// StoredEvent is an Event as persisted, augmented with server-assigned fields.
type StoredEvent struct {
	Event
	ID         int64     `json:"id"`
	ReceivedAt time.Time `json:"received_at"`
}
