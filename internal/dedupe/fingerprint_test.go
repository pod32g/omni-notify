package dedupe

import (
	"testing"

	"github.com/pod32g/omni-notify/internal/models"
)

func TestFingerprint_UsesProvided(t *testing.T) {
	ev := models.Event{Fingerprint: "explicit-key", Type: "alert", Source: "x", EventID: "y"}
	if got := Fingerprint(ev); got != "explicit-key" {
		t.Fatalf("expected provided fingerprint, got %q", got)
	}
}

func TestFingerprint_StableAndLabelOrderIndependent(t *testing.T) {
	a := models.Event{Type: "alert", Source: "homelab", EventID: "pihole-down",
		Labels: map[string]string{"service": "pihole", "host": "rpi"}}
	b := models.Event{Type: "alert", Source: "homelab", EventID: "pihole-down",
		Labels: map[string]string{"host": "rpi", "service": "pihole"}}

	fa, fb := Fingerprint(a), Fingerprint(b)
	if fa != fb {
		t.Fatalf("label order changed fingerprint: %q vs %q", fa, fb)
	}
	if len(fa) != 64 {
		t.Fatalf("expected 64-hex sha256, got len %d", len(fa))
	}
}

func TestFingerprint_DiffersOnFieldChange(t *testing.T) {
	base := models.Event{Type: "alert", Source: "homelab", EventID: "x",
		Labels: map[string]string{"k": "v"}}
	variants := []models.Event{
		{Type: "deploy", Source: "homelab", EventID: "x", Labels: map[string]string{"k": "v"}},
		{Type: "alert", Source: "other", EventID: "x", Labels: map[string]string{"k": "v"}},
		{Type: "alert", Source: "homelab", EventID: "y", Labels: map[string]string{"k": "v"}},
		{Type: "alert", Source: "homelab", EventID: "x", Labels: map[string]string{"k": "w"}},
	}
	want := Fingerprint(base)
	for i, v := range variants {
		if Fingerprint(v) == want {
			t.Errorf("variant %d produced same fingerprint as base", i)
		}
	}
}

// TestFingerprint_NoBoundaryCollision guards against field-concatenation ambiguity.
func TestFingerprint_NoBoundaryCollision(t *testing.T) {
	a := models.Event{Type: "ab", Source: "c", EventID: "d"}
	b := models.Event{Type: "a", Source: "bc", EventID: "d"}
	if Fingerprint(a) == Fingerprint(b) {
		t.Fatal("field boundary collision between ('ab','c') and ('a','bc')")
	}
}
