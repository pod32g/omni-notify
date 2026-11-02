package notifier

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pod32g/omni-notify/internal/dedupe"
	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/providers"
)

// Deliver records ev and queues one delivery to each named provider, skipping
// routing, deduplication and state. It is how plugins send what they decide to
// send (an on-call page to one person's channels); the queue, retries and
// delivery records are the same as for routed events. Because routes are never
// consulted, an event delivered this way can't loop back into a plugin.
func (n *Notifier) Deliver(ctx context.Context, ev models.Event, route string, providerNames []string) error {
	if len(providerNames) == 0 {
		return fmt.Errorf("no providers to deliver to")
	}
	ev.Normalize()
	if err := ev.Validate(); err != nil {
		return err
	}
	if err := providers.ValidateMessage(ev.Message); err != nil {
		return err
	}
	for _, name := range providerNames {
		if _, err := n.store.GetProvider(ctx, name); err != nil {
			return fmt.Errorf("provider %q: %w", name, err)
		}
	}
	ev.Fingerprint = dedupe.Fingerprint(ev)
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	stored, err := n.store.InsertEvent(ctx, ev, raw)
	if err != nil {
		return err
	}
	n.metrics.EventsReceived.WithLabelValues(string(ev.Severity), string(ev.Status)).Inc()
	now := n.clock.Now().UTC()
	seen := map[string]bool{}
	for _, name := range providerNames {
		if seen[name] {
			continue
		}
		seen[name] = true
		if _, err := n.store.EnqueueDelivery(ctx, models.Delivery{
			Fingerprint:   ev.Fingerprint,
			EventRef:      stored.ID,
			Route:         route,
			Provider:      name,
			MaxAttempts:   n.cfg.MaxAttempts,
			NextAttemptAt: now,
		}); err != nil {
			return err
		}
	}
	n.nudge()
	return nil
}
