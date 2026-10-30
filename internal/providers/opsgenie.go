package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/pod32g/omni-notify/internal/models"
)

// opsgenie creates and closes alerts via the Opsgenie Alert API v2.
//
// secret: the Opsgenie API key (required), sent as "GenieKey <key>".
// config:
//   - api_url: the alerts endpoint (optional, default
//     "https://api.opsgenie.com/v2/alerts"). Override for EU/sandbox regions
//     or testing.
//
// The alert alias is the event Fingerprint (falling back to EventID), so a
// resolved event closes the matching open alert by alias.
type opsgenie struct {
	client *http.Client
	apiURL string
	apiKey string
}

const opsgenieDefaultAPIURL = "https://api.opsgenie.com/v2/alerts"

func newOpsgenie(client *http.Client, cfg map[string]any, secret string, allowPrivate bool) (Provider, error) {
	if secret == "" {
		return nil, fmt.Errorf("opsgenie: API key secret is required")
	}
	apiURL := cfgStringDefault(cfg, "api_url", opsgenieDefaultAPIURL)
	if err := validateHTTPURL(apiURL, allowPrivate); err != nil {
		return nil, fmt.Errorf("opsgenie: %w", err)
	}
	return &opsgenie{client: client, apiURL: apiURL, apiKey: secret}, nil
}

type opsgenieCreate struct {
	Message     string            `json:"message"`
	Description string            `json:"description,omitempty"`
	Alias       string            `json:"alias,omitempty"`
	Source      string            `json:"source,omitempty"`
	Priority    string            `json:"priority,omitempty"`
	Tags        []string          `json:"tags,omitempty"`
	Details     map[string]string `json:"details,omitempty"`
	Actions     []string          `json:"actions,omitempty"`
	Entity      string            `json:"entity,omitempty"`
	Note        string            `json:"note,omitempty"`
	User        string            `json:"user,omitempty"`
	Responders  []json.RawMessage `json:"responders,omitempty"`
	VisibleTo   []json.RawMessage `json:"visibleTo,omitempty"`
}

type opsgenieClose struct {
	Source string `json:"source"`
	Note   string `json:"note,omitempty"`
}

func (o *opsgenie) Send(ctx context.Context, msg models.NotificationMessage) error {
	ev := msg.Event
	ov := overrideOf(ev, "opsgenie", parseOpsgenieOverride)
	alias := ev.Fingerprint
	if alias == "" {
		alias = ev.EventID
	}
	if ov.Alias != "" {
		alias = ov.Alias
	}
	headers := map[string]string{"Authorization": "GenieKey " + o.apiKey}
	if ev.Status == models.StatusResolved {
		closeURL := o.apiURL + "/" + url.PathEscape(alias) + "/close?identifierType=alias"
		_, err := sendJSON(ctx, o.client, http.MethodPost, closeURL, opsgenieClose{Source: "omni-notify", Note: ov.Note}, headers)
		return err
	}
	plain := opsgenieCreate{
		Message:     truncate(headline(ev), 130),
		Description: bodyText(ev),
		Alias:       alias,
		Source:      ev.Source,
	}
	if isEmptyOverride(ov) {
		_, err := sendJSON(ctx, o.client, http.MethodPost, o.apiURL, plain, headers)
		return err
	}
	rich := plain
	if ov.Message != "" {
		rich.Message = ov.Message
	}
	if ov.Description != "" {
		rich.Description = ov.Description
	}
	rich.Priority, rich.Tags, rich.Details, rich.Actions = ov.Priority, ov.Tags, ov.Details, ov.Actions
	rich.Entity, rich.Note, rich.User = ov.Entity, ov.Note, ov.User
	rich.Responders, rich.VisibleTo = ov.Responders, ov.VisibleTo
	_, err := sendJSON(ctx, o.client, http.MethodPost, o.apiURL, rich, headers)
	if isPayloadRefused(err) {
		slog.Warn("opsgenie: sending the plain alert instead", "err", err)
		_, err = sendJSON(ctx, o.client, http.MethodPost, o.apiURL, plain, headers)
	}
	return err
}
