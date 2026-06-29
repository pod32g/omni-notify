// Package providers defines the pluggable notification Provider interface, a
// registry of provider kinds, and the built-in Discord, Slack, generic webhook
// and SMTP implementations.
package providers

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
)

// Provider delivers a notification message through a specific channel.
type Provider interface {
	Send(ctx context.Context, msg models.NotificationMessage) error
}

// Constructor builds a Provider from its stored (non-secret) config and its
// decrypted secret. It must validate inputs and return a descriptive error.
type Constructor func(cfg map[string]any, secret string) (Provider, error)

// Registry maps provider kinds to constructors.
type Registry struct {
	mu    sync.RWMutex
	ctors map[string]Constructor
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{ctors: make(map[string]Constructor)}
}

// Register adds (or replaces) a constructor for a kind.
func (r *Registry) Register(kind string, c Constructor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ctors[kind] = c
}

// Has reports whether a kind is registered.
func (r *Registry) Has(kind string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.ctors[kind]
	return ok
}

// Kinds returns the registered kinds, sorted.
func (r *Registry) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.ctors))
	for k := range r.ctors {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Build constructs a provider of the given kind.
func (r *Registry) Build(kind string, cfg map[string]any, secret string) (Provider, error) {
	r.mu.RLock()
	c, ok := r.ctors[kind]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown provider kind %q", kind)
	}
	return c(cfg, secret)
}

// NewDefault returns a registry with all built-in providers registered, sharing
// httpClient for the HTTP-based providers. If httpClient is nil a guarded client
// honouring allowPrivate is created. allowPrivate also controls build-time
// rejection of literal private/loopback target hosts.
func NewDefault(httpClient *http.Client, allowPrivate bool) *Registry {
	if httpClient == nil {
		httpClient = NewGuardedClient(15*time.Second, allowPrivate)
	}
	r := NewRegistry()
	r.Register("discord", func(cfg map[string]any, secret string) (Provider, error) {
		return newDiscord(httpClient, cfg, secret, allowPrivate)
	})
	r.Register("slack", func(cfg map[string]any, secret string) (Provider, error) {
		return newSlack(httpClient, cfg, secret, allowPrivate)
	})
	r.Register("webhook", func(cfg map[string]any, secret string) (Provider, error) {
		return newWebhook(httpClient, cfg, secret, allowPrivate)
	})
	r.Register("smtp", func(cfg map[string]any, secret string) (Provider, error) {
		return newSMTP(cfg, secret)
	})
	r.Register("telegram", func(cfg map[string]any, secret string) (Provider, error) {
		return newTelegram(httpClient, cfg, secret, allowPrivate)
	})
	r.Register("ntfy", func(cfg map[string]any, secret string) (Provider, error) {
		return newNtfy(httpClient, cfg, secret, allowPrivate)
	})
	r.Register("gotify", func(cfg map[string]any, secret string) (Provider, error) {
		return newGotify(httpClient, cfg, secret, allowPrivate)
	})
	r.Register("pushover", func(cfg map[string]any, secret string) (Provider, error) {
		return newPushover(httpClient, cfg, secret, allowPrivate)
	})
	r.Register("teams", func(cfg map[string]any, secret string) (Provider, error) {
		return newTeams(httpClient, cfg, secret, allowPrivate)
	})
	r.Register("matrix", func(cfg map[string]any, secret string) (Provider, error) {
		return newMatrix(httpClient, cfg, secret, allowPrivate)
	})
	r.Register("pagerduty", func(cfg map[string]any, secret string) (Provider, error) {
		return newPagerDuty(httpClient, cfg, secret, allowPrivate)
	})
	r.Register("opsgenie", func(cfg map[string]any, secret string) (Provider, error) {
		return newOpsgenie(httpClient, cfg, secret, allowPrivate)
	})
	r.Register("googlechat", func(cfg map[string]any, secret string) (Provider, error) {
		return newGoogleChat(httpClient, cfg, secret, allowPrivate)
	})
	r.Register("twilio", func(cfg map[string]any, secret string) (Provider, error) {
		return newTwilio(httpClient, cfg, secret, allowPrivate)
	})
	return r
}
