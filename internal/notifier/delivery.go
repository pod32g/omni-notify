package notifier

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime/debug"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/storage"
)

// dispatchBatchSize bounds how many deliveries are claimed per dispatcher pass.
const dispatchBatchSize = 64

// Start launches the dispatcher and worker pool. It returns once goroutines are
// running; call Stop to shut down gracefully. ctx cancellation also stops it.
func (n *Notifier) Start(ctx context.Context) error {
	if reset, err := n.store.ResetInProgress(context.Background()); err != nil {
		return fmt.Errorf("reset in-progress deliveries: %w", err)
	} else if reset > 0 {
		n.log.Info("recovered stuck deliveries", "count", reset)
	}
	n.refreshActiveGauge(context.Background())

	n.jobs = make(chan models.Delivery, n.cfg.QueueSize)
	n.doneCh = make(chan struct{})

	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		n.runWorkers(ctx)
	}()
	go func() {
		defer close(n.doneCh)
		n.dispatch(ctx)
		<-workerDone
	}()
	return nil
}

// Stop waits for the delivery engine to finish after its context is cancelled.
// Callers cancel the context passed to Start, then call Stop to block until done.
func (n *Notifier) Stop() {
	if n.doneCh != nil {
		<-n.doneCh
	}
}

// dispatch polls storage for due deliveries and feeds them to workers. It closes
// the jobs channel on exit so workers drain and stop.
func (n *Notifier) dispatch(ctx context.Context) {
	defer close(n.jobs)
	ticker := time.NewTicker(n.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		n.dispatchBatch(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-n.wake:
		}
	}
}

// dispatchBatch claims and enqueues all currently due deliveries.
func (n *Notifier) dispatchBatch(ctx context.Context) {
	for {
		claimed, err := n.store.ClaimDueDeliveries(ctx, dispatchBatchSize)
		if err != nil {
			if ctx.Err() == nil {
				n.log.Error("claim deliveries", "err", err)
			}
			return
		}
		if len(claimed) == 0 {
			return
		}
		for _, d := range claimed {
			select {
			case n.jobs <- d:
			case <-ctx.Done():
				// Remaining claimed rows stay in_progress; recovered on next boot.
				return
			}
		}
		if len(claimed) < dispatchBatchSize {
			return
		}
	}
}

// runWorkers starts cfg.Workers goroutines consuming the jobs channel.
func (n *Notifier) runWorkers(ctx context.Context) {
	done := make(chan struct{})
	for i := 0; i < n.cfg.Workers; i++ {
		go func() {
			for d := range n.jobs {
				// On shutdown, stop processing and let claimed rows be recovered
				// from in_progress on the next boot rather than burning an attempt.
				if ctx.Err() != nil {
					continue
				}
				n.safeDeliver(ctx, d)
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < n.cfg.Workers; i++ {
		<-done
	}
}

// safeDeliver runs deliver with panic recovery so a single misbehaving provider
// cannot crash the whole process.
func (n *Notifier) safeDeliver(ctx context.Context, d models.Delivery) {
	defer func() {
		if r := recover(); r != nil {
			n.log.Error("delivery panic", "id", d.ID, "provider", d.Provider,
				"panic", r, "stack", string(debug.Stack()))
			n.markDead(context.Background(), d, d.AttemptCount+1, "unknown", fmt.Sprintf("panic: %v", r))
		}
	}()
	n.deliver(ctx, d)
}

// deliver performs a single delivery attempt and records the outcome, scheduling
// a retry with exponential backoff on failure or marking it dead when exhausted.
// The send is bound to ctx so a shutdown aborts in-flight requests; storage
// updates intentionally use a background context so the outcome is still recorded.
func (n *Notifier) deliver(ctx context.Context, d models.Delivery) {
	bg := context.Background()
	attempt := d.AttemptCount + 1

	pcfg, err := n.store.GetProvider(bg, d.Provider)
	if errors.Is(err, storage.ErrNotFound) {
		n.markDead(bg, d, attempt, "unknown", fmt.Sprintf("provider %q not found", d.Provider))
		return
	}
	if err != nil {
		n.scheduleRetryOrDie(bg, d, attempt, "unknown", err, 0)
		return
	}
	if !pcfg.Enabled {
		n.markDead(bg, d, attempt, pcfg.Kind, fmt.Sprintf("provider %q is disabled", d.Provider))
		return
	}

	prov, err := n.registry.Build(pcfg.Kind, pcfg.Config, pcfg.Secret)
	if err != nil {
		n.markDead(bg, d, attempt, pcfg.Kind, fmt.Sprintf("build provider: %v", err))
		return
	}

	stored, err := n.store.GetEvent(bg, d.EventRef)
	if err != nil {
		n.markDead(bg, d, attempt, pcfg.Kind, fmt.Sprintf("load event %d: %v", d.EventRef, err))
		return
	}

	msg := models.NotificationMessage{Event: stored.Event, Route: d.Route}
	sendCtx, cancel := context.WithTimeout(ctx, n.cfg.SendTimeout)
	start := time.Now()
	sendErr := prov.Send(sendCtx, msg)
	dur := time.Since(start)
	cancel()

	n.metrics.DeliveryDuration.WithLabelValues(pcfg.Kind).Observe(dur.Seconds())

	if sendErr == nil {
		if err := n.store.MarkDeliverySuccess(bg, d.ID, attempt, dur); err != nil {
			n.log.Error("mark delivery success", "id", d.ID, "err", err)
		}
		n.metrics.NotificationsSent.WithLabelValues(pcfg.Kind).Inc()
		n.log.Info("delivered", "fingerprint", d.Fingerprint, "route", d.Route,
			"provider", d.Provider, "attempt", attempt, "duration_ms", dur.Milliseconds())
		return
	}

	n.metrics.ProviderErrors.WithLabelValues(pcfg.Kind).Inc()
	n.scheduleRetryOrDie(bg, d, attempt, pcfg.Kind, sendErr, dur)
}

// scheduleRetryOrDie marks a delivery for retry with backoff, or dead when it has
// no attempts left.
func (n *Notifier) scheduleRetryOrDie(ctx context.Context, d models.Delivery, attempt int, kind string, sendErr error, dur time.Duration) {
	if attempt >= d.MaxAttempts {
		n.markDead(ctx, d, attempt, kind, sendErr.Error())
		return
	}
	next := n.clock.Now().UTC().Add(n.backoff(attempt))
	if err := n.store.MarkDeliveryRetry(ctx, d.ID, attempt, next, sendErr.Error(), dur); err != nil {
		n.log.Error("mark delivery retry", "id", d.ID, "err", err)
	}
	n.log.Warn("delivery failed, will retry", "fingerprint", d.Fingerprint, "route", d.Route,
		"provider", d.Provider, "attempt", attempt, "next_attempt", next, "err", sendErr)
}

// markDead records a permanently failed delivery and counts it.
func (n *Notifier) markDead(ctx context.Context, d models.Delivery, attempt int, kind, msg string) {
	if err := n.store.MarkDeliveryDead(ctx, d.ID, attempt, msg, 0); err != nil {
		n.log.Error("mark delivery dead", "id", d.ID, "err", err)
	}
	n.metrics.NotificationsFailed.WithLabelValues(kind).Inc()
	n.log.Error("delivery permanently failed", "fingerprint", d.Fingerprint, "route", d.Route,
		"provider", d.Provider, "attempt", attempt, "err", msg)
}

// backoff returns the delay before the next attempt (1-based) using exponential
// growth capped at BackoffMax.
func (n *Notifier) backoff(attempt int) time.Duration {
	base := float64(n.cfg.BackoffBase)
	if base <= 0 {
		base = float64(time.Second)
	}
	factor := n.cfg.BackoffFactor
	if factor < 1 {
		factor = 2
	}
	d := base * math.Pow(factor, float64(attempt-1))
	if max := float64(n.cfg.BackoffMax); max > 0 && d > max {
		d = max
	}
	return time.Duration(d)
}

// TestProvider sends a synthetic notification through a named provider
// synchronously, returning any send error. Used by POST /api/v1/test.
func (n *Notifier) TestProvider(ctx context.Context, name string) error {
	pcfg, err := n.store.GetProvider(ctx, name)
	if err != nil {
		return err
	}
	prov, err := n.registry.Build(pcfg.Kind, pcfg.Config, pcfg.Secret)
	if err != nil {
		return fmt.Errorf("build provider: %w", err)
	}
	msg := models.NotificationMessage{
		Event: models.Event{
			EventID:   "omni-notify-test",
			Type:      "test",
			Source:    "omni-notify",
			Severity:  models.SeverityInfo,
			Title:     "Omni-Notify Test Notification",
			Summary:   "If you can see this, the provider is configured correctly.",
			Timestamp: n.clock.Now().UTC(),
		},
		Route: "test",
		Test:  true,
	}
	sendCtx, cancel := context.WithTimeout(ctx, n.cfg.SendTimeout)
	defer cancel()
	return prov.Send(sendCtx, msg)
}
