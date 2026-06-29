package models

import "time"

// State is the current tracked state of a logical event, keyed by fingerprint.
// It powers GET /api/v1/states and reflects the most recently observed status.
type State struct {
	Fingerprint string            `json:"fingerprint"`
	EventID     string            `json:"event_id"`
	Type        string            `json:"type"`
	Source      string            `json:"source"`
	Status      Status            `json:"status"`
	Severity    Severity          `json:"severity,omitempty"`
	Title       string            `json:"title"`
	Labels      map[string]string `json:"labels,omitempty"`
	Active      bool              `json:"active"`
	FirstSeen   time.Time         `json:"first_seen"`
	LastSeen    time.Time         `json:"last_seen"`
}

// RouteDedup is the per-(fingerprint, route) notification bookkeeping that drives
// the notify/suppress decision while honouring each route's own window/repeat.
type RouteDedup struct {
	Fingerprint    string
	Route          string
	LastStatus     Status
	Active         bool
	RepeatCount    int
	LastNotifiedAt time.Time
}
