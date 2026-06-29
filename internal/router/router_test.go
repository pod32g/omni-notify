package router

import (
	"sort"
	"testing"

	"github.com/pod32g/omni-notify/internal/models"
)

func names(routes []models.Route) []string {
	out := make([]string, len(routes))
	for i, r := range routes {
		out[i] = r.Name
	}
	sort.Strings(out)
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestMatch(t *testing.T) {
	routes := []models.Route{
		{Name: "critical", Match: map[string]string{"severity": "critical"}},
		{Name: "security", Match: map[string]string{"source": "omni-identity", "type": "security"}},
		{Name: "pihole", Match: map[string]string{"labels.service": "pihole"}},
		{Name: "disabled", Match: map[string]string{"severity": "critical"}, Disabled: true},
		{Name: "fallback", IsDefault: true},
	}

	ev := func(mut func(*models.Event)) models.Event {
		e := models.Event{Type: "alert", Source: "homelab", Title: "t"}
		mut(&e)
		return e
	}

	tests := []struct {
		name string
		ev   models.Event
		want []string
	}{
		{"critical severity", ev(func(e *models.Event) { e.Severity = models.SeverityCritical }), []string{"critical"}},
		{"security multi-condition", ev(func(e *models.Event) { e.Source = "omni-identity"; e.Type = "security" }), []string{"security"}},
		{"label match", ev(func(e *models.Event) { e.Labels = map[string]string{"service": "pihole"} }), []string{"pihole"}},
		{"multiple matches", ev(func(e *models.Event) {
			e.Severity = models.SeverityCritical
			e.Labels = map[string]string{"service": "pihole"}
		}), []string{"critical", "pihole"}},
		{"no match falls back to default", ev(func(e *models.Event) { e.Type = "deploy" }), []string{"fallback"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := names(Match(tt.ev, routes))
			if !eq(got, tt.want) {
				t.Errorf("Match = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatch_DisabledNeverReturned(t *testing.T) {
	routes := []models.Route{{Name: "off", Disabled: true, Match: map[string]string{"type": "alert"}}}
	if got := Match(models.Event{Type: "alert"}, routes); len(got) != 0 {
		t.Fatalf("disabled route returned: %v", names(got))
	}
}

func TestMatch_DefaultIgnoredWhenNonDefaultMatches(t *testing.T) {
	routes := []models.Route{
		{Name: "specific", Match: map[string]string{"type": "alert"}},
		{Name: "fallback", IsDefault: true},
	}
	got := names(Match(models.Event{Type: "alert"}, routes))
	if !eq(got, []string{"specific"}) {
		t.Fatalf("expected only specific, got %v", got)
	}
}

// orderedNames returns route names in their returned order (not sorted).
func orderedNames(routes []models.Route) []string {
	out := make([]string, len(routes))
	for i, r := range routes {
		out[i] = r.Name
	}
	return out
}

func TestMatch_PriorityOrdering(t *testing.T) {
	m := map[string]string{"type": "alert"}
	routes := []models.Route{
		{Name: "low", Match: m, Priority: 1},
		{Name: "high", Match: m, Priority: 10},
		{Name: "mid", Match: m, Priority: 5},
	}
	got := orderedNames(Match(models.Event{Type: "alert"}, routes))
	want := []string{"high", "mid", "low"}
	if !eq(got, want) {
		t.Fatalf("priority order = %v, want %v", got, want)
	}
}

func TestMatch_PriorityTieBrokenByName(t *testing.T) {
	m := map[string]string{"type": "alert"}
	routes := []models.Route{
		{Name: "zebra", Match: m, Priority: 5},
		{Name: "alpha", Match: m, Priority: 5},
	}
	got := orderedNames(Match(models.Event{Type: "alert"}, routes))
	if !eq(got, []string{"alpha", "zebra"}) {
		t.Fatalf("tie-break order = %v, want [alpha zebra]", got)
	}
}

func TestMatch_StopProcessing(t *testing.T) {
	m := map[string]string{"type": "alert"}
	routes := []models.Route{
		{Name: "stopper", Match: m, Priority: 10, StopProcessing: true},
		{Name: "peer", Match: m, Priority: 10}, // same tier: still applies
		{Name: "lower", Match: m, Priority: 5}, // strictly lower: dropped
	}
	got := orderedNames(Match(models.Event{Type: "alert"}, routes))
	want := []string{"peer", "stopper"} // sorted within tier by name
	if !eq(got, want) {
		t.Fatalf("stop_processing result = %v, want %v", got, want)
	}
}

func TestMatch_StopProcessingKeepsHigher(t *testing.T) {
	m := map[string]string{"type": "alert"}
	routes := []models.Route{
		{Name: "top", Match: m, Priority: 20},
		{Name: "stopper", Match: m, Priority: 10, StopProcessing: true},
		{Name: "lower", Match: m, Priority: 5},
	}
	got := orderedNames(Match(models.Event{Type: "alert"}, routes))
	want := []string{"top", "stopper"} // higher kept, lower dropped
	if !eq(got, want) {
		t.Fatalf("result = %v, want %v", got, want)
	}
}

func TestMatch_AnnotationKeys(t *testing.T) {
	routes := []models.Route{{Name: "rb", Match: map[string]string{"annotations.runbook": "yes"}}}
	ev := models.Event{Type: "alert", Annotations: map[string]string{"runbook": "yes"}}
	if got := names(Match(ev, routes)); !eq(got, []string{"rb"}) {
		t.Fatalf("annotation match failed: %v", got)
	}
}
