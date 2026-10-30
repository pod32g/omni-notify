// Package plugin is the seam between Omni-Notify's core and its optional
// plugins. A plugin is compiled in, off unless config enables it, and owns its
// own data and HTTP routes. It reaches the core only through Host: it can add
// provider kinds (so routes deliver events to it) and ask for notifications to
// be delivered through named providers. The core never imports a plugin.
package plugin

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"

	"github.com/pod32g/omni-notify/internal/clock"
	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/providers"
	"gopkg.in/yaml.v3"
)

// Plugin is one optional part of Omni-Notify.
type Plugin interface {
	// Name is the plugin's config key and URL prefix: its routes are served
	// under /<name>/.
	Name() string
	// Start opens the plugin's storage, registers its provider kinds and starts
	// its background work. It runs once, before deliveries and the HTTP server.
	Start(ctx context.Context, host Host) error
	// Handler serves the plugin's routes with the /<name> prefix stripped. The
	// core does not authenticate them; the plugin does (see Host.IsAdminToken).
	Handler() http.Handler
	// Stop ends background work and closes storage.
	Stop()
}

// Factory builds a plugin from its config section.
type Factory func(cfg yaml.Node) (Plugin, error)

// Deliverer sends ev through exactly the named providers, skipping routing and
// deduplication, with the core's queue, retries and delivery records. route
// names the sender in delivery records.
type Deliverer func(ctx context.Context, ev models.Event, route string, providers []string) error

// Host is what the core lends a plugin.
type Host struct {
	Log       *slog.Logger
	Clock     clock.Clock
	Providers *providers.Registry
	Deliver   Deliverer
	// IsAdminToken reports whether a bearer token is one of Notify's API tokens.
	IsAdminToken func(token string) bool
}

// TokenMatcher returns a constant-time check against the configured tokens.
func TokenMatcher(tokens []string) func(string) bool {
	return func(presented string) bool {
		if presented == "" {
			return false
		}
		ok := false
		for _, t := range tokens {
			if subtle.ConstantTimeCompare([]byte(presented), []byte(t)) == 1 {
				ok = true
			}
		}
		return ok
	}
}
