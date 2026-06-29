package models

import "time"

// Route maps matching events to a set of providers. Match keys are exact-string
// matched against event fields; supported keys are type, source, severity,
// status, and dotted labels.<k> / annotations.<k>.
type Route struct {
	Name      string            `json:"name"`
	Match     map[string]string `json:"match,omitempty"`
	Providers []string          `json:"providers"`
	IsDefault bool              `json:"is_default"`
	Disabled  bool              `json:"disabled"`
	// Priority orders route evaluation: higher priority evaluates first. Routes
	// with equal priority are ordered by name for determinism.
	Priority int `json:"priority"`
	// StopProcessing, when true on a matching route, prevents evaluation of
	// strictly lower-priority routes. Same-priority routes still evaluate.
	StopProcessing bool      `json:"stop_processing"`
	DedupWindow    Duration  `json:"dedup_window,omitempty"`
	RepeatInterval Duration  `json:"repeat_interval,omitempty"`
	ManagedBy      ManagedBy `json:"managed_by"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}
