package providers

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pod32g/omni-notify/internal/models"
)

// subjectLine builds a one-line summary like "[CRITICAL] Pi-hole Down".
func subjectLine(ev models.Event) string {
	tag := tagFor(ev)
	if tag == "" {
		return ev.Title
	}
	return fmt.Sprintf("[%s] %s", tag, ev.Title)
}

// tagFor chooses the most informative prefix tag from severity then status.
func tagFor(ev models.Event) string {
	if ev.Severity != models.SeverityNone {
		return strings.ToUpper(string(ev.Severity))
	}
	if ev.Status != models.StatusNone {
		return strings.ToUpper(string(ev.Status))
	}
	return ""
}

// plainBody renders a human-readable multi-line body used by SMTP and as a
// fallback elsewhere.
func plainBody(ev models.Event) string {
	var b strings.Builder
	if ev.Summary != "" {
		b.WriteString(ev.Summary)
		b.WriteString("\n\n")
	}
	if ev.Description != "" {
		b.WriteString(ev.Description)
		b.WriteString("\n\n")
	}
	writeField(&b, "Source", ev.Source)
	writeField(&b, "Type", ev.Type)
	if ev.Status != models.StatusNone {
		writeField(&b, "Status", string(ev.Status))
	}
	if ev.Severity != models.SeverityNone {
		writeField(&b, "Severity", string(ev.Severity))
	}
	writeField(&b, "Event ID", ev.EventID)
	writeField(&b, "Time", ev.Timestamp.UTC().Format("2006-01-02 15:04:05 MST"))
	for _, kv := range sortedPairs(ev.Labels) {
		writeField(&b, "label:"+kv[0], kv[1])
	}
	for _, kv := range sortedPairs(ev.Annotations) {
		writeField(&b, kv[0], kv[1])
	}
	return strings.TrimRight(b.String(), "\n")
}

func writeField(b *strings.Builder, k, v string) {
	if v == "" {
		return
	}
	fmt.Fprintf(b, "%s: %s\n", k, v)
}

// sortedPairs returns map entries sorted by key for stable rendering.
func sortedPairs(m map[string]string) [][2]string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([][2]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, [2]string{k, m[k]})
	}
	return out
}

// statusColorHex maps an event to an embed colour (RGB int) for rich providers.
func statusColorHex(ev models.Event) int {
	switch ev.Status {
	case models.StatusResolved:
		return 0x2ECC71 // green
	case models.StatusFiring:
		return 0xE74C3C // red
	}
	switch ev.Severity {
	case models.SeverityCritical, models.SeverityError:
		return 0xE74C3C // red
	case models.SeverityWarning:
		return 0xF1C40F // yellow
	case models.SeverityInfo:
		return 0x3498DB // blue
	case models.SeverityDebug:
		return 0x95A5A6 // grey
	}
	return 0x95A5A6 // grey
}

// slackColor maps an event to a Slack attachment colour.
func slackColor(ev models.Event) string {
	switch {
	case ev.Status == models.StatusResolved:
		return "good"
	case ev.Status == models.StatusFiring, ev.Severity == models.SeverityCritical, ev.Severity == models.SeverityError:
		return "danger"
	case ev.Severity == models.SeverityWarning:
		return "warning"
	default:
		return "#3498DB"
	}
}

// detailFields returns label/annotation key-value pairs for structured display.
func detailFields(ev models.Event) [][2]string {
	var out [][2]string
	if ev.Source != "" {
		out = append(out, [2]string{"Source", ev.Source})
	}
	if ev.Status != models.StatusNone {
		out = append(out, [2]string{"Status", string(ev.Status)})
	}
	if ev.Severity != models.SeverityNone {
		out = append(out, [2]string{"Severity", string(ev.Severity)})
	}
	out = append(out, sortedPairs(ev.Labels)...)
	return out
}
