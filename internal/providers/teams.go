package providers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/pod32g/omni-notify/internal/models"
)

// teams posts a legacy MessageCard to a Microsoft Teams incoming webhook.
//
// secret: the Teams incoming webhook URL (required).
// config:
//   - url: override the webhook URL (defaults to the secret); primarily for
//     testing against a local capture server.
type teams struct {
	client *http.Client
	url    string
}

func newTeams(client *http.Client, cfg map[string]any, secret string, allowPrivate bool) (Provider, error) {
	if secret == "" {
		return nil, fmt.Errorf("teams: webhook URL secret is required")
	}
	target := cfgStringDefault(cfg, "url", secret)
	if err := validateHTTPURL(target, allowPrivate); err != nil {
		return nil, fmt.Errorf("teams: %w", err)
	}
	return &teams{client: client, url: target}, nil
}

// teamsCard is the legacy MessageCard payload Teams renders.
type teamsCard struct {
	Type       string `json:"@type"`
	Context    string `json:"@context"`
	ThemeColor string `json:"themeColor"`
	Summary    string `json:"summary"`
	Title      string `json:"title"`
	Text       string `json:"text"`
}

func (t *teams) Send(ctx context.Context, msg models.NotificationMessage) error {
	ev := msg.Event
	subject := headline(ev)
	if subject == "" {
		subject = "Notification"
	}
	card := teamsCard{
		Type:       "MessageCard",
		Context:    "http://schema.org/extensions",
		ThemeColor: fmt.Sprintf("%06X", statusColorHex(ev)),
		Summary:    subject,
		Title:      subject,
		Text:       bodyText(ev),
	}
	ov := overrideOf(ev, "teams", parseTeamsOverride)
	var rich any
	switch {
	case len(ov.Card) > 0:
		rich = map[string]any{
			"type": "message",
			"attachments": []map[string]any{{
				"contentType": "application/vnd.microsoft.card.adaptive",
				"contentUrl":  nil,
				"content":     ov.Card,
			}},
		}
	case len(ov.MessageCard) > 0:
		rich = ov.MessageCard
	default:
		_, err := sendJSON(ctx, t.client, http.MethodPost, t.url, card, nil)
		return err
	}
	_, err := sendJSON(ctx, t.client, http.MethodPost, t.url, rich, nil)
	if isPayloadRefused(err) {
		slog.Warn("teams: sending the plain card instead", "err", err)
		_, err = sendJSON(ctx, t.client, http.MethodPost, t.url, card, nil)
	}
	return err
}
