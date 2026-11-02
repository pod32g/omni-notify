package providers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

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
	plain := googleChatPayload{Text: fullText(ev, "\n")}
	ov := overrideOf(ev, "googlechat", parseGoogleChatOverride)
	if isEmptyOverride(ov) {
		_, err := sendJSON(ctx, g.client, http.MethodPost, g.url, plain, nil)
		return err
	}
	rich := map[string]any{"text": plain.Text}
	if ov.Text != "" {
		rich["text"] = ov.Text
	}
	if len(ov.CardsV2) > 0 {
		rich["cardsV2"] = ov.CardsV2
		if ov.Text == "" && !ev.Message.Custom() {
			delete(rich, "text") // the cards are the message
		}
	}
	target := g.url
	if ov.ThreadKey != "" {
		rich["thread"] = map[string]string{"threadKey": ov.ThreadKey}
		target = withQuery(g.url, "messageReplyOption", "REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD")
	}
	_, err := sendJSON(ctx, g.client, http.MethodPost, target, rich, nil)
	if isPayloadRefused(err) {
		slog.Warn("googlechat: sending the plain message instead", "err", err)
		_, err = sendJSON(ctx, g.client, http.MethodPost, g.url, plain, nil)
	}
	return err
}

// withQuery returns raw with the query parameter k set to v.
func withQuery(raw, k, v string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set(k, v)
	u.RawQuery = q.Encode()
	return u.String()
}
