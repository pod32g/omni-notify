package dedupe

import (
	"testing"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
)

func TestDecide(t *testing.T) {
	now := time.Date(2026, 6, 28, 20, 0, 0, 0, time.UTC)
	const fp, route = "fp1", "r1"

	prev := func(active bool, status models.Status, notifiedAgo time.Duration, repeat int) models.RouteDedup {
		return models.RouteDedup{
			Fingerprint:    fp,
			Route:          route,
			Active:         active,
			LastStatus:     status,
			RepeatCount:    repeat,
			LastNotifiedAt: now.Add(-notifiedAgo),
		}
	}

	tests := []struct {
		name       string
		status     models.Status
		prev       models.RouteDedup
		prevExists bool
		pol        Policy
		wantNotify bool
		wantReason Reason
		wantActive bool
	}{
		{
			name: "firing new", status: models.StatusFiring, prevExists: false,
			wantNotify: true, wantReason: ReasonNewFiring, wantActive: true,
		},
		{
			name: "firing again after resolved", status: models.StatusFiring,
			prev: prev(false, models.StatusResolved, time.Hour, 0), prevExists: true,
			wantNotify: true, wantReason: ReasonNewFiring, wantActive: true,
		},
		{
			name: "firing repeat suppressed (no repeat interval)", status: models.StatusFiring,
			prev: prev(true, models.StatusFiring, time.Minute, 0), prevExists: true,
			pol:        Policy{RepeatInterval: 0},
			wantNotify: false, wantReason: ReasonSuppressRepeat, wantActive: true,
		},
		{
			name: "firing repeat suppressed (within interval)", status: models.StatusFiring,
			prev: prev(true, models.StatusFiring, 5*time.Minute, 1), prevExists: true,
			pol:        Policy{RepeatInterval: time.Hour},
			wantNotify: false, wantReason: ReasonSuppressRepeat, wantActive: true,
		},
		{
			name: "firing repeat notified (interval elapsed)", status: models.StatusFiring,
			prev: prev(true, models.StatusFiring, 2*time.Hour, 1), prevExists: true,
			pol:        Policy{RepeatInterval: time.Hour},
			wantNotify: true, wantReason: ReasonRepeatInterval, wantActive: true,
		},
		{
			name: "resolved when active", status: models.StatusResolved,
			prev: prev(true, models.StatusFiring, time.Minute, 0), prevExists: true,
			wantNotify: true, wantReason: ReasonResolved, wantActive: false,
		},
		{
			name: "resolved when never fired", status: models.StatusResolved, prevExists: false,
			wantNotify: false, wantReason: ReasonSuppressResolve, wantActive: false,
		},
		{
			name: "stateless first time", status: models.StatusNone, prevExists: false,
			pol:        Policy{DedupWindow: 5 * time.Minute},
			wantNotify: true, wantReason: ReasonStatelessNotify,
		},
		{
			name: "stateless within window", status: models.StatusNone,
			prev: prev(false, models.StatusNone, time.Minute, 1), prevExists: true,
			pol:        Policy{DedupWindow: 5 * time.Minute},
			wantNotify: false, wantReason: ReasonSuppressWindow,
		},
		{
			name: "stateless after window", status: models.StatusNone,
			prev: prev(false, models.StatusNone, 10*time.Minute, 1), prevExists: true,
			pol:        Policy{DedupWindow: 5 * time.Minute},
			wantNotify: true, wantReason: ReasonStatelessNotify,
		},
		{
			name: "stateless no window always notifies", status: models.StatusNone,
			prev: prev(false, models.StatusNone, time.Second, 3), prevExists: true,
			pol:        Policy{DedupWindow: 0},
			wantNotify: true, wantReason: ReasonStatelessNotify,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.prev
			p.Fingerprint, p.Route = fp, route
			got := Decide(models.Event{Status: tt.status, Fingerprint: fp}, p, tt.prevExists, tt.pol, now)
			if got.Notify != tt.wantNotify {
				t.Errorf("Notify = %v, want %v", got.Notify, tt.wantNotify)
			}
			if got.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tt.wantReason)
			}
			if got.State.Active != tt.wantActive {
				t.Errorf("State.Active = %v, want %v", got.State.Active, tt.wantActive)
			}
			if got.State.Fingerprint != fp || got.State.Route != route {
				t.Errorf("State identity not preserved: %+v", got.State)
			}
			if tt.wantNotify && !got.State.LastNotifiedAt.Equal(now) {
				t.Errorf("expected LastNotifiedAt=now on notify, got %v", got.State.LastNotifiedAt)
			}
		})
	}
}

// TestDecide_StatelessPreservesActiveResolve guards the bug where a stateless
// event sharing a fingerprint+route with a firing alert would clear Active and
// cause a later resolve to be wrongly suppressed.
func TestDecide_StatelessPreservesActiveResolve(t *testing.T) {
	now := time.Date(2026, 6, 28, 20, 0, 0, 0, time.UTC)
	pol := Policy{DedupWindow: 0} // stateless always notifies
	const fp, route = "fp1", "r1"

	// 1. firing -> active
	d1 := Decide(models.Event{Status: models.StatusFiring, Fingerprint: fp},
		models.RouteDedup{Fingerprint: fp, Route: route}, false, pol, now)
	if !d1.State.Active {
		t.Fatal("firing should set active")
	}

	// 2. a stateless info for the same fingerprint+route arrives
	d2 := Decide(models.Event{Status: models.StatusNone, Fingerprint: fp}, d1.State, true, pol, now)
	if !d2.State.Active {
		t.Fatal("stateless event must preserve the active flag")
	}

	// 3. resolve must still notify
	d3 := Decide(models.Event{Status: models.StatusResolved, Fingerprint: fp}, d2.State, true, pol, now)
	if !d3.Notify || d3.Reason != ReasonResolved {
		t.Fatalf("resolve after stateless should notify, got notify=%v reason=%q", d3.Notify, d3.Reason)
	}
}
