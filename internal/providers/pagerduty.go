package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/pod32g/omni-notify/internal/models"
)

// pagerduty triggers and resolves incidents via the PagerDuty Events API v2.
//
// secret: the routing/integration key (required).
// config:
//   - events_url: the Events API endpoint
//     (optional, default "https://events.pagerduty.com/v2/enqueue").
//   - source: the alert source (optional, default the event's Source).
type pagerduty struct {
	client     *http.Client
	url        string
	routingKey string
	source     string
}

func newPagerDuty(client *http.Client, cfg map[string]any, secret string, allowPrivate bool) (Provider, error) {
	if secret == "" {
		return nil, fmt.Errorf("pagerduty: routing key secret is required")
	}
	eventsURL := cfgStringDefault(cfg, "events_url", "https://events.pagerduty.com/v2/enqueue")
	if err := validateHTTPURL(eventsURL, allowPrivate); err != nil {
		return nil, fmt.Errorf("pagerduty: %w", err)
	}
	return &pagerduty{
		client:     client,
		url:        eventsURL,
		routingKey: secret,
		source:     cfgString(cfg, "source"),
	}, nil
}

type pagerDutyPayload struct {
	Summary       string          `json:"summary"`
	Source        string          `json:"source"`
	Severity      string          `json:"severity"`
	Component     string          `json:"component,omitempty"`
	Group         string          `json:"group,omitempty"`
	Class         string          `json:"class,omitempty"`
	CustomDetails json.RawMessage `json:"custom_details,omitempty"`
}

type pagerDutyEvent struct {
	RoutingKey  string            `json:"routing_key"`
	EventAction string            `json:"event_action"`
	DedupKey    string            `json:"dedup_key,omitempty"`
	Payload     *pagerDutyPayload `json:"payload,omitempty"`
	Links       []pdLink          `json:"links,omitempty"`
	Images      []pdImage         `json:"images,omitempty"`
	Client      string            `json:"client,omitempty"`
	ClientURL   string            `json:"client_url,omitempty"`
}

// pagerDutySeverity maps an event severity to a PagerDuty severity value.
func pagerDutySeverity(ev models.Event) string {
	switch ev.Severity {
	case models.SeverityCritical:
		return "critical"
	case models.SeverityError:
		return "error"
	case models.SeverityWarning:
		return "warning"
	default:
		return "info"
	}
}

func (p *pagerduty) Send(ctx context.Context, msg models.NotificationMessage) error {
	ev := msg.Event

	dedupKey := ev.Fingerprint
	if dedupKey == "" {
		dedupKey = ev.EventID
	}

	payload := pagerDutyEvent{
		RoutingKey: p.routingKey,
		DedupKey:   dedupKey,
	}

	if ev.Status == models.StatusResolved {
		payload.EventAction = "resolve"
	} else {
		source := p.source
		if source == "" {
			source = ev.Source
		}
		payload.EventAction = "trigger"
		payload.Payload = &pagerDutyPayload{
			Summary:  truncate(headline(ev), 1024),
			Source:   source,
			Severity: pagerDutySeverity(ev),
		}
	}

	ov := overrideOf(ev, "pagerduty", parsePagerDutyOverride)
	if isEmptyOverride(ov) {
		_, err := sendJSON(ctx, p.client, http.MethodPost, p.url, payload, nil)
		return err
	}
	rich := payload
	if ov.DedupKey != "" {
		rich.DedupKey = ov.DedupKey
	}
	if ov.EventAction != "" {
		rich.EventAction = ov.EventAction
	}
	if rich.EventAction == "trigger" {
		pl := pagerDutyPayload{Summary: truncate(headline(ev), 1024), Source: ev.Source, Severity: pagerDutySeverity(ev)}
		if p.source != "" {
			pl.Source = p.source
		}
		if payload.Payload != nil {
			pl = *payload.Payload
		}
		if ov.Summary != "" {
			pl.Summary = ov.Summary
		}
		if ov.Severity != "" {
			pl.Severity = ov.Severity
		}
		pl.Component, pl.Group, pl.Class, pl.CustomDetails = ov.Component, ov.Group, ov.Class, ov.CustomDetails
		rich.Payload = &pl
		rich.Links, rich.Images, rich.Client, rich.ClientURL = ov.Links, ov.Images, ov.Client, ov.ClientURL
	} else {
		rich.Payload = nil // acknowledge and resolve carry no payload
	}
	_, err := sendJSON(ctx, p.client, http.MethodPost, p.url, rich, nil)
	if isPayloadRefused(err) {
		slog.Warn("pagerduty: sending the plain event instead", "err", err)
		_, err = sendJSON(ctx, p.client, http.MethodPost, p.url, payload, nil)
	}
	return err
}
