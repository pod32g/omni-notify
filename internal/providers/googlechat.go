package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/pod32g/omni-notify/internal/models"
)

// googleChat posts plain-text messages to a Google Chat incoming webhook.
//
// secret: the incoming webhook URL (required).
// config:
//   - url: optional override for the webhook URL (defaults to the secret); used
//     mainly so tests can target a local server.
type googleChat struct {
	client *http.Client
	url    string
}

func newGoogleChat(client *http.Client, cfg map[string]any, secret string, allowPrivate bool) (Provider, error) {
	url := cfgStringDefault(cfg, "url", secret)
	if url == "" {
		return nil, fmt.Errorf("googlechat: webhook URL secret is required")
	}
	if err := validateHTTPURL(url, allowPrivate); err != nil {
		return nil, fmt.Errorf("googlechat: %w", err)
	}
	return &googleChat{client: client, url: url}, nil
}

// googleChatPayload is the minimal incoming-webhook message body.
type googleChatPayload struct {
	Text string `json:"text"`
}

func (g *googleChat) Send(ctx context.Context, msg models.NotificationMessage) error {
	ev := msg.Event
	text := subjectLine(ev) + "\n" + plainBody(ev)
	body, err := json.Marshal(googleChatPayload{Text: text})
	if err != nil {
		return fmt.Errorf("googlechat: marshal: %w", err)
	}
	return postBytes(ctx, g.client, http.MethodPost, g.url, "application/json", body, nil)
}
