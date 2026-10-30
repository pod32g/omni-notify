package notifier

import (
	"context"
	"testing"

	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/storage"
)

func TestDeliver_SkipsRoutingAndQueuesTheNamedProviders(t *testing.T) {
	h := newHarness(t, Config{})
	ctx := context.Background()
	// No route matches this event; Deliver must not need one.
	if err := h.store.UpsertRoute(ctx, models.Route{Name: "all", Disabled: true, Providers: []string{"stub"}}); err != nil {
		t.Fatal(err)
	}
	ev := firingEvent()
	ev.Status = models.StatusNone
	if err := h.n.Deliver(ctx, ev, "oncall", []string{"stub", "stub"}); err != nil {
		t.Fatal(err)
	}
	ds, err := h.store.ListDeliveries(ctx, storage.DeliveryFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].Provider != "stub" || ds[0].Route != "oncall" {
		t.Fatalf("deliveries %+v, want one to stub from route oncall", ds)
	}
	if err := h.n.Deliver(ctx, ev, "oncall", []string{"missing"}); err == nil {
		t.Fatal("an unknown provider should be refused")
	}
	if err := h.n.Deliver(ctx, models.Event{}, "oncall", []string{"stub"}); err == nil {
		t.Fatal("an invalid event should be refused")
	}
}
