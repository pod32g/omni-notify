package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/pod32g/omni-notify/internal/models"
)

func TestNewPagerDuty_Trigger(t *testing.T) {
	srv, body := captureServer(t, http.StatusAccepted)
	cfg := map[string]any{"events_url": srv.URL}
	p, err := newPagerDuty(srv.Client(), cfg, "routing-key-123", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err != nil {
		t.Fatal(err)
	}
	var payload pagerDutyEvent
	if err := json.Unmarshal(*body, &payload); err != nil {
		t.Fatalf("invalid pagerduty payload: %v\n%s", err, *body)
	}
	if payload.RoutingKey != "routing-key-123" {
		t.Fatalf("routing_key not set: %+v", payload)
	}
	if payload.EventAction != "trigger" {
		t.Fatalf("event_action = %q, want trigger", payload.EventAction)
	}
	if payload.DedupKey != "pihole-down" {
		t.Fatalf("dedup_key = %q, want pihole-down (EventID fallback)", payload.DedupKey)
	}
	if payload.Payload == nil || payload.Payload.Severity != "critical" {
		t.Fatalf("unexpected payload: %+v", payload.Payload)
	}
	if payload.Payload.Source != "homelab" {
		t.Fatalf("source = %q, want homelab", payload.Payload.Source)
	}
}

func TestNewPagerDuty_Resolve(t *testing.T) {
	srv, body := captureServer(t, http.StatusAccepted)
	cfg := map[string]any{"events_url": srv.URL}
	p, err := newPagerDuty(srv.Client(), cfg, "routing-key-123", true)
	if err != nil {
		t.Fatal(err)
	}
	ev := testEvent()
	ev.Status = models.StatusResolved
	ev.Fingerprint = "fp-abc"
	if err := p.Send(context.Background(), models.NotificationMessage{Event: ev}); err != nil {
		t.Fatal(err)
	}
	var payload pagerDutyEvent
	if err := json.Unmarshal(*body, &payload); err != nil {
		t.Fatalf("invalid pagerduty payload: %v\n%s", err, *body)
	}
	if payload.EventAction != "resolve" {
		t.Fatalf("event_action = %q, want resolve", payload.EventAction)
	}
	if payload.DedupKey != "fp-abc" {
		t.Fatalf("dedup_key = %q, want fp-abc (Fingerprint)", payload.DedupKey)
	}
	if payload.Payload != nil {
		t.Fatalf("resolve must not include payload: %+v", payload.Payload)
	}
}

func TestNewPagerDuty_RequiresSecret(t *testing.T) {
	if _, err := newPagerDuty(http.DefaultClient, nil, "", true); err == nil {
		t.Fatal("expected error for missing routing key")
	}
}

func TestNewPagerDuty_RejectsBadURL(t *testing.T) {
	cfg := map[string]any{"events_url": "ftp://example.com/x"}
	if _, err := newPagerDuty(http.DefaultClient, cfg, "key", true); err == nil {
		t.Fatal("expected error for non-http events_url")
	}
}
