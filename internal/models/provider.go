package models

import "time"

// ManagedBy records whether an entity was seeded from the YAML config or created
// through the API. Config-managed entities are re-synced from config on boot.
type ManagedBy string

const (
	ManagedByConfig ManagedBy = "config"
	ManagedByAPI    ManagedBy = "api"
)

// ProviderConfig is a stored, named provider instance. The Secret holds the
// sensitive value (webhook URL, SMTP password) and is encrypted at rest; it is
// never serialised to API responses (json:"-").
type ProviderConfig struct {
	Name      string         `json:"name"`
	Kind      string         `json:"kind"`
	Config    map[string]any `json:"config,omitempty"`
	Secret    string         `json:"-"`
	Enabled   bool           `json:"enabled"`
	ManagedBy ManagedBy      `json:"managed_by"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// HasSecret reports whether a secret is set, for safe display on the API.
func (p ProviderConfig) HasSecret() bool { return p.Secret != "" }
