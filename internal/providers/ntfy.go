package providers

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/pod32g/omni-notify/internal/models"
)

// ntfy publishes notifications to an ntfy.sh topic (or a self-hosted ntfy
// server) via a plain-text HTTP POST to the topic URL.
//
// secret: the full topic publish URL, e.g. "https://ntfy.sh/alerts" (required).
// config:
//   - priority: optional message priority 1-5 (sent as the "Priority" header)
//   - tags:     optional comma-separated tags (sent as the "Tags" header)
//   - token:    optional bearer token (sent as "Authorization: Bearer <token>")
type ntfy struct {
	client   *http.Client
	url      string
	priority int
	tags     string
	token    string
}

func newNtfy(client *http.Client, cfg map[string]any, secret string, allowPrivate bool) (Provider, error) {
	if secret == "" {
		return nil, fmt.Errorf("ntfy: topic publish URL secret is required")
	}
	if err := validateHTTPURL(secret, allowPrivate); err != nil {
		return nil, fmt.Errorf("ntfy: %w", err)
	}
	priority := cfgInt(cfg, "priority", 0)
	if priority != 0 && (priority < 1 || priority > 5) {
		return nil, fmt.Errorf("ntfy: priority must be between 1 and 5, got %d", priority)
	}
	return &ntfy{
		client:   client,
		url:      secret,
		priority: priority,
		tags:     cfgString(cfg, "tags"),
		token:    cfgString(cfg, "token"),
	}, nil
}

func (n *ntfy) Send(ctx context.Context, msg models.NotificationMessage) error {
	ev := msg.Event
	title := subjectLine(ev)
	if msg.Test {
		title = "Omni-Notify test notification"
	}
	body := plainBody(ev)

	headers := map[string]string{
		"Title": sanitizeHeader(title),
	}
	if n.priority != 0 {
		headers["Priority"] = strconv.Itoa(n.priority)
	}
	if n.tags != "" {
		headers["Tags"] = sanitizeHeader(n.tags)
	}
	if n.token != "" {
		headers["Authorization"] = "Bearer " + sanitizeHeader(n.token)
	}
	return postBytes(ctx, n.client, http.MethodPost, n.url, "text/plain", []byte(body), headers)
}
