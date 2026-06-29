// Package notifier orchestrates event ingestion (fingerprint, state, dedup,
// routing, enqueue) and runs the durable, retrying delivery worker pool.
package notifier

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/pod32g/omni-notify/internal/clock"
	"github.com/pod32g/omni-notify/internal/dedupe"
	"github.com/pod32g/omni-notify/internal/metrics"
	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/providers"
	"github.com/pod32g/omni-notify/internal/router"
	"github.com/pod32g/omni-notify/internal/storage"
)

// Config controls the delivery engine and default deduplication policy.
type Config struct {
	Workers               int
	QueueSize             int
	MaxAttempts           int
	BackoffBase           time.Duration
	BackoffFactor         float64
	BackoffMax            time.Duration
	SendTimeout           time.Duration
	PollInterval          time.Duration
	DefaultDedupWindow    time.Duration
	DefaultRepeatInterval time.Duration
}

// Notifier ties storage, providers and metrics together.
type Notifier struct {
	store    *storage.Store
	registry *providers.Registry
	metrics  *metrics.Metrics
	clock    clock.Clock
	log      *slog.Logger
	cfg      Config

	jobs    chan models.Delivery
	wake    chan struct{}
	doneCh  chan struct{}
	fpLocks shardedMutex
}

// New constructs a Notifier. A nil logger discards output; a nil clock uses real time.
func New(store *storage.Store, registry *providers.Registry, m *metrics.Metrics, clk clock.Clock, log *slog.Logger, cfg Config) *Notifier {
	if clk == nil {
		clk = clock.Real{}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 256
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.SendTimeout <= 0 {
		cfg.SendTimeout = 10 * time.Second
	}
	return &Notifier{
		store:    store,
		registry: registry,
		metrics:  m,
		clock:    clk,
		log:      log,
		cfg:      cfg,
		wake:     make(chan struct{}, 1),
	}
}

// ProcessResult summarises what happened to an ingested event.
type ProcessResult struct {
	Fingerprint        string   `json:"fingerprint"`
	EventID            int64    `json:"event_id"`
	Deduplicated       bool     `json:"deduplicated"`
	RoutesMatched      []string `json:"routes_matched"`
	DeliveriesEnqueued int      `json:"deliveries_enqueued"`
}

// Process validates-free ingestion: it assumes ev has already been validated. It
// derives the fingerprint, stores the event, updates state, evaluates routes and
// per-route deduplication, and enqueues deliveries for routes that should notify.
func (n *Notifier) Process(ctx context.Context, ev models.Event, raw []byte) (ProcessResult, error) {
	fp := dedupe.Fingerprint(ev)
	ev.Fingerprint = fp
	now := n.clock.Now().UTC()

	// Serialize all processing for a given fingerprint so the per-route
	// read-modify-write of dedup state (Get -> Decide -> Upsert -> Enqueue) is
	// atomic against concurrent ingestion of the same event. SQLite's single
	// writer only serializes individual statements, not this multi-step decision.
	unlock := n.fpLocks.lock(fp)
	defer unlock.Unlock()

	stored, err := n.store.InsertEvent(ctx, ev, raw)
	if err != nil {
		return ProcessResult{}, err
	}
	n.metrics.EventsReceived.WithLabelValues(string(ev.Severity), string(ev.Status)).Inc()

	if err := n.updateState(ctx, ev, fp, now); err != nil {
		return ProcessResult{}, err
	}

	routes, err := n.store.ListRoutes(ctx)
	if err != nil {
		return ProcessResult{}, err
	}
	matched := router.Match(ev, routes)

	res := ProcessResult{Fingerprint: fp, EventID: stored.ID, RoutesMatched: []string{}}
	// seenProvider collapses providers to exactly one delivery per provider across
	// all notifying routes. Routes arrive in priority order, so the first
	// (highest-priority) notifying route that names a provider owns its delivery.
	seenProvider := map[string]bool{}
	anyNotified := false

	for _, r := range matched {
		res.RoutesMatched = append(res.RoutesMatched, r.Name)

		prev, exists, err := n.store.GetRouteDedup(ctx, fp, r.Name)
		if err != nil {
			return ProcessResult{}, err
		}
		dec := dedupe.Decide(ev, prev, exists, n.effectivePolicy(r), now)
		if err := n.store.UpsertRouteDedup(ctx, dec.State); err != nil {
			return ProcessResult{}, err
		}
		if !dec.Notify {
			n.metrics.EventsDeduplicated.WithLabelValues(r.Name).Inc()
			n.log.Debug("suppressed", "fingerprint", fp, "route", r.Name, "reason", string(dec.Reason))
			continue
		}
		anyNotified = true
		for _, pname := range r.Providers {
			if seenProvider[pname] {
				continue
			}
			seenProvider[pname] = true
			if _, err := n.store.EnqueueDelivery(ctx, models.Delivery{
				Fingerprint:   fp,
				EventRef:      stored.ID,
				Route:         r.Name,
				Provider:      pname,
				MaxAttempts:   n.cfg.MaxAttempts,
				NextAttemptAt: now,
			}); err != nil {
				return ProcessResult{}, err
			}
			res.DeliveriesEnqueued++
		}
	}

	res.Deduplicated = len(matched) > 0 && !anyNotified
	n.nudge()
	n.refreshActiveGauge(ctx)
	return res, nil
}

// updateState refreshes the per-fingerprint event state, preserving first_seen.
func (n *Notifier) updateState(ctx context.Context, ev models.Event, fp string, now time.Time) error {
	existing, err := n.store.GetState(ctx, fp)
	exists := err == nil
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	firstSeen := now
	if exists {
		firstSeen = existing.FirstSeen
	}
	return n.store.UpsertState(ctx, models.State{
		Fingerprint: fp,
		EventID:     ev.EventID,
		Type:        ev.Type,
		Source:      ev.Source,
		Status:      ev.Status,
		Severity:    ev.Severity,
		Title:       ev.Title,
		Labels:      ev.Labels,
		Active:      computeActive(ev.Status, existing, exists),
		FirstSeen:   firstSeen,
		LastSeen:    now,
	})
}

// computeActive derives the active flag from the new status, falling back to the
// previous value for stateless events.
func computeActive(status models.Status, prev models.State, prevExists bool) bool {
	switch status {
	case models.StatusFiring:
		return true
	case models.StatusResolved:
		return false
	default:
		return prevExists && prev.Active
	}
}

// effectivePolicy resolves a route's dedup policy against the configured
// defaults. A route value of zero means "inherit the default"; a negative value
// explicitly disables the behaviour (window/interval of 0), which is otherwise
// unreachable because the configured default would always win.
func (n *Notifier) effectivePolicy(r models.Route) dedupe.Policy {
	return dedupe.Policy{
		DedupWindow:    resolvePolicyField(r.DedupWindow.D(), n.cfg.DefaultDedupWindow),
		RepeatInterval: resolvePolicyField(r.RepeatInterval.D(), n.cfg.DefaultRepeatInterval),
	}
}

func resolvePolicyField(routeVal, def time.Duration) time.Duration {
	switch {
	case routeVal < 0:
		return 0 // explicitly disabled
	case routeVal == 0:
		return def // inherit default
	default:
		return routeVal
	}
}

// refreshActiveGauge updates the active-states gauge from storage.
func (n *Notifier) refreshActiveGauge(ctx context.Context) {
	count, err := n.store.CountActiveStates(ctx)
	if err != nil {
		n.log.Warn("count active states", "err", err)
		return
	}
	n.metrics.ActiveStates.Set(float64(count))
}

// nudge wakes the dispatcher without blocking.
func (n *Notifier) nudge() {
	select {
	case n.wake <- struct{}{}:
	default:
	}
}
