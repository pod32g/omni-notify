package models

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEventValidate(t *testing.T) {
	valid := Event{EventID: "e", Type: "alert", Source: "s", Title: "t", Timestamp: time.Now()}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}

	tests := []struct {
		name string
		mut  func(*Event)
	}{
		{"missing event_id", func(e *Event) { e.EventID = "" }},
		{"missing type", func(e *Event) { e.Type = "" }},
		{"missing source", func(e *Event) { e.Source = "" }},
		{"missing title", func(e *Event) { e.Title = "" }},
		{"missing timestamp", func(e *Event) { e.Timestamp = time.Time{} }},
		{"bad status", func(e *Event) { e.Status = "exploded" }},
		{"bad severity", func(e *Event) { e.Severity = "nuclear" }},
		{"empty label key", func(e *Event) { e.Labels = map[string]string{"": "v"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := valid
			tt.mut(&e)
			if err := e.Validate(); err == nil {
				t.Errorf("expected validation error for %s", tt.name)
			}
		})
	}
}

func TestStatusIsStateful(t *testing.T) {
	for _, s := range []Status{StatusFiring, StatusResolved} {
		if !s.IsStateful() {
			t.Errorf("%q should be stateful", s)
		}
	}
	for _, s := range []Status{StatusNone, Status("info")} {
		if s.IsStateful() {
			t.Errorf("%q should not be stateful", s)
		}
	}
}

func TestStatusValid(t *testing.T) {
	for _, s := range []Status{StatusNone, StatusFiring, StatusResolved} {
		if !s.Valid() {
			t.Errorf("%q should be valid status", s)
		}
	}
	// Legacy severity-as-status values are no longer valid statuses.
	for _, s := range []Status{"info", "warning", "error", "bogus"} {
		if s.Valid() {
			t.Errorf("%q should not be a valid status", s)
		}
	}
}

func TestSeverityValid(t *testing.T) {
	for _, sv := range []Severity{SeverityNone, SeverityCritical, SeverityError, SeverityWarning, SeverityInfo, SeverityDebug} {
		if !sv.Valid() {
			t.Errorf("%q should be valid severity", sv)
		}
	}
	if Severity("nuclear").Valid() {
		t.Error("nuclear should not be valid severity")
	}
}

func TestNormalizeMigratesLegacyStatus(t *testing.T) {
	cases := []struct {
		inStatus   Status
		inSeverity Severity
		wantStatus Status
		wantSev    Severity
	}{
		{"warning", SeverityNone, StatusNone, SeverityWarning},
		{"error", SeverityNone, StatusNone, SeverityError},
		{"info", SeverityNone, StatusNone, SeverityInfo},
		{"critical", SeverityNone, StatusNone, SeverityCritical},
		{"debug", SeverityNone, StatusNone, SeverityDebug},
		{"warning", SeverityCritical, StatusNone, SeverityCritical}, // existing severity wins
		{StatusFiring, SeverityCritical, StatusFiring, SeverityCritical},
		{StatusResolved, SeverityNone, StatusResolved, SeverityNone},
	}
	for _, c := range cases {
		e := Event{Status: c.inStatus, Severity: c.inSeverity}
		e.Normalize()
		if e.Status != c.wantStatus || e.Severity != c.wantSev {
			t.Errorf("Normalize(%q,%q) = (%q,%q), want (%q,%q)",
				c.inStatus, c.inSeverity, e.Status, e.Severity, c.wantStatus, c.wantSev)
		}
	}
}

func TestDurationJSON(t *testing.T) {
	type wrap struct {
		D Duration `json:"d"`
	}
	// string form
	var w wrap
	if err := json.Unmarshal([]byte(`{"d":"5m"}`), &w); err != nil {
		t.Fatal(err)
	}
	if w.D.D() != 5*time.Minute {
		t.Fatalf("got %v, want 5m", w.D.D())
	}
	// number form (seconds)
	if err := json.Unmarshal([]byte(`{"d":30}`), &w); err != nil {
		t.Fatal(err)
	}
	if w.D.D() != 30*time.Second {
		t.Fatalf("got %v, want 30s", w.D.D())
	}
	// round-trip
	b, err := json.Marshal(wrap{D: Duration(90 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	var back wrap
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.D.D() != 90*time.Second {
		t.Fatalf("round-trip lost value: %v", back.D.D())
	}
}

func TestDurationYAMLString(t *testing.T) {
	var d Duration
	err := d.UnmarshalYAML(func(v any) error {
		*(v.(*any)) = "2h"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.D() != 2*time.Hour {
		t.Fatalf("got %v, want 2h", d.D())
	}
}
