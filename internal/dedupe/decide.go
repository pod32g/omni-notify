package dedupe

import (
	"time"

	"github.com/pod32g/omni-notify/internal/models"
)

// Reason explains a deduplication decision (useful for logging and metrics).
type Reason string

const (
	ReasonNewFiring       Reason = "new_firing"
	ReasonRepeatInterval  Reason = "repeat_interval"
	ReasonResolved        Reason = "resolved"
	ReasonStatelessNotify Reason = "stateless_notify"
	ReasonSuppressRepeat  Reason = "suppressed_repeat"
	ReasonSuppressResolve Reason = "suppressed_resolve_no_active"
	ReasonSuppressWindow  Reason = "suppressed_window"
)

// Policy is the effective per-route deduplication policy. The notifier resolves
// route-level overrides against config defaults before calling Decide, so a
// zero DedupWindow here means "no stateless deduplication" and a zero
// RepeatInterval means "never re-notify an already-firing event".
type Policy struct {
	DedupWindow    time.Duration
	RepeatInterval time.Duration
}

// Decision is the outcome of evaluating one event against one route's dedup state.
type Decision struct {
	Notify bool
	Reason Reason
	// State is the dedup record to persist regardless of Notify.
	State models.RouteDedup
}

// Decide determines whether to notify for an event on a given route, given the
// previous per-(fingerprint, route) dedup record (prevExists=false for none).
//
// Rules:
//   - firing: notify if not currently active; if active, notify only when the
//     repeat interval has elapsed.
//   - resolved: notify once if it was active, then mark inactive; suppress a
//     resolve for something that never fired.
//   - stateless (info/warning/error/none): notify if the dedup window has
//     elapsed since the last notification (or there was none).
func Decide(ev models.Event, prev models.RouteDedup, prevExists bool, pol Policy, now time.Time) Decision {
	fp, route := prev.Fingerprint, prev.Route

	base := func(active bool, status models.Status, repeat int, notifiedAt time.Time) models.RouteDedup {
		return models.RouteDedup{
			Fingerprint:    fp,
			Route:          route,
			LastStatus:     status,
			Active:         active,
			RepeatCount:    repeat,
			LastNotifiedAt: notifiedAt,
		}
	}

	switch ev.Status {
	case models.StatusFiring:
		if !prevExists || !prev.Active {
			return Decision{true, ReasonNewFiring, base(true, models.StatusFiring, 0, now)}
		}
		if pol.RepeatInterval > 0 && !now.Before(prev.LastNotifiedAt.Add(pol.RepeatInterval)) {
			return Decision{true, ReasonRepeatInterval, base(true, models.StatusFiring, prev.RepeatCount+1, now)}
		}
		// Still firing but within the repeat window: keep prior bookkeeping.
		return Decision{false, ReasonSuppressRepeat, base(true, models.StatusFiring, prev.RepeatCount, prev.LastNotifiedAt)}

	case models.StatusResolved:
		if prevExists && prev.Active {
			return Decision{true, ReasonResolved, base(false, models.StatusResolved, 0, now)}
		}
		return Decision{false, ReasonSuppressResolve, base(false, models.StatusResolved, prev.RepeatCount, prev.LastNotifiedAt)}

	default: // stateless: info, warning, error, none
		// Preserve any firing/resolved active flag: a stateless event may share a
		// fingerprint+route with a stateful alert, and clobbering Active=false here
		// would cause a later resolve to be wrongly suppressed.
		active := prevExists && prev.Active
		if !prevExists || pol.DedupWindow <= 0 || prev.LastNotifiedAt.IsZero() ||
			!now.Before(prev.LastNotifiedAt.Add(pol.DedupWindow)) {
			repeat := 0
			if prevExists {
				repeat = prev.RepeatCount + 1
			}
			return Decision{true, ReasonStatelessNotify, base(active, ev.Status, repeat, now)}
		}
		return Decision{false, ReasonSuppressWindow, base(active, ev.Status, prev.RepeatCount, prev.LastNotifiedAt)}
	}
}
