// Package router selects which routes an event should be delivered through,
// using exact matching on event fields with priority ordering and a
// default-route fallback.
//
// Resolution algorithm (deterministic):
//
//  1. Collect non-disabled routes whose match conditions all hold.
//  2. If none match, fall back to matching default routes.
//  3. Sort by priority (descending), then name (ascending) for stable order.
//  4. If a matched route has stop_processing, drop all strictly lower-priority
//     routes (same-priority routes still apply).
//
// The caller (notifier) then evaluates per-route deduplication in this order and
// collapses the resulting providers to one delivery each.
package router

import (
	"sort"
	"strings"

	"github.com/pod32g/omni-notify/internal/models"
)

// Match returns the routes that apply to ev, ordered by the resolution algorithm
// documented on the package. Disabled routes are never returned.
func Match(ev models.Event, routes []models.Route) []models.Route {
	var matched, defaults []models.Route
	for _, r := range routes {
		if r.Disabled {
			continue
		}
		if !matchesAll(ev, r.Match) {
			continue
		}
		if r.IsDefault {
			defaults = append(defaults, r)
		} else {
			matched = append(matched, r)
		}
	}
	selected := matched
	if len(selected) == 0 {
		selected = defaults
	}
	sortByPriority(selected)
	return applyStopProcessing(selected)
}

// sortByPriority orders routes by priority descending, then name ascending.
func sortByPriority(routes []models.Route) {
	sort.SliceStable(routes, func(i, j int) bool {
		if routes[i].Priority != routes[j].Priority {
			return routes[i].Priority > routes[j].Priority
		}
		return routes[i].Name < routes[j].Name
	})
}

// applyStopProcessing truncates the sorted route list at the first route that
// sets stop_processing: routes at the same priority are kept, strictly
// lower-priority routes are dropped.
func applyStopProcessing(sorted []models.Route) []models.Route {
	for i, r := range sorted {
		if r.StopProcessing {
			cutoff := r.Priority
			j := i + 1
			for j < len(sorted) && sorted[j].Priority == cutoff {
				j++
			}
			return sorted[:j]
		}
	}
	return sorted
}

// matchesAll reports whether the event satisfies every condition in match.
// An empty match matches all events.
func matchesAll(ev models.Event, match map[string]string) bool {
	for key, want := range match {
		got, ok := fieldValue(ev, key)
		if !ok || got != want {
			return false
		}
	}
	return true
}

// fieldValue resolves a match key against the event. Supported keys are the
// top-level fields type/source/severity/status/event_id/title and the dotted
// forms labels.<k> and annotations.<k>.
func fieldValue(ev models.Event, key string) (string, bool) {
	switch key {
	case "type":
		return ev.Type, true
	case "source":
		return ev.Source, true
	case "severity":
		return string(ev.Severity), true
	case "status":
		return string(ev.Status), true
	case "event_id":
		return ev.EventID, true
	case "title":
		return ev.Title, true
	}
	if k, ok := strings.CutPrefix(key, "labels."); ok {
		v, present := ev.Labels[k]
		return v, present
	}
	if k, ok := strings.CutPrefix(key, "annotations."); ok {
		v, present := ev.Annotations[k]
		return v, present
	}
	return "", false
}
