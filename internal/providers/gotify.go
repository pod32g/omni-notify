package providers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/pod32g/omni-notify/internal/models"
)

// gotify pushes notifications to a self-hosted Gotify server.
//
// secret: the application token (required), sent as the X-Gotify-Key header.
// config:
//   - url:      Gotify server base URL (required); the message endpoint is
//     <url>/message.
//   - priority: optional message priority (default 5).
type gotify struct {
	client   *http.Client
	url      string
	token    string
	priority int
}

func newGotify(client *http.Client, cfg map[string]any, secret string, allowPrivate bool) (Provider, error) {
	if secret == "" {
		return nil, fmt.Errorf("gotify: application token secret is required")
	}
	base := cfgString(cfg, "url")
	if base == "" {
		return nil, fmt.Errorf("gotify: url config is required")
	}
	target := strings.TrimRight(base, "/") + "/message"
	if err := validateHTTPURL(target, allowPrivate); err != nil {
		return nil, fmt.Errorf("gotify: %w", err)
	}
	return &gotify{
		client:   client,
		url:      target,
		token:    secret,
		priority: cfgInt(cfg, "priority", 5),
	}, nil
}

type gotifyPayload struct {
	Title    string         `json:"title"`
	Message  string         `json:"message"`
	Priority int            `json:"priority"`
	Extras   map[string]any `json:"extras,omitempty"`
}

func (g *gotify) Send(ctx context.Context, msg models.NotificationMessage) error {
	plain := gotifyPayload{
		Title:    headline(msg.Event),
		Message:  bodyText(msg.Event),
		Priority: g.priority,
	}
	headers := map[string]string{"X-Gotify-Key": sanitizeHeader(g.token)}
	ov := overrideOf(msg.Event, "gotify", parseGotifyOverride)
	if isEmptyOverride(ov) {
		_, err := sendJSON(ctx, g.client, http.MethodPost, g.url, plain, headers)
		return err
	}
	rich := plain
	if ov.Title != "" {
		rich.Title = ov.Title
	}
	if ov.Message != "" {
		rich.Message = ov.Message
	}
	if ov.Priority != nil {
		rich.Priority = *ov.Priority
	}
	if extras := ov.extras(); len(extras) > 0 {
		rich.Extras = extras
	}
	_, err := sendJSON(ctx, g.client, http.MethodPost, g.url, rich, headers)
	if isPayloadRefused(err) {
		slog.Warn("gotify: sending the plain message instead", "err", err)
		_, err = sendJSON(ctx, g.client, http.MethodPost, g.url, plain, headers)
	}
	return err
}
