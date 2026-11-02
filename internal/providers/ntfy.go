package providers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

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
	ov := overrideOf(msg.Event, "ntfy", parseNtfyOverride)
	if isEmptyOverride(ov) {
		return n.sendPlain(ctx, msg)
	}
	if ov.Send == nil || *ov.Send {
		err := n.publishJSON(ctx, msg, ov)
		if isPayloadRefused(err) {
			slog.Warn("ntfy: sending the plain message instead", "err", err)
			err = n.sendPlain(ctx, msg)
		}
		if err != nil {
			return err
		}
	}
	return n.dismiss(ctx, ov, ov.Send != nil && !*ov.Send)
}

// publishJSON publishes the message with ntfy's JSON API: to the server's root
// URL, naming the topic.
func (n *ntfy) publishJSON(ctx context.Context, msg models.NotificationMessage, ov ntfyOverride) error {
	ev := msg.Event
	root, topic := n.rootAndTopic()
	p := map[string]any{"topic": topic, "title": headline(ev), "message": bodyText(ev)}
	if msg.Test {
		p["title"] = "Omni-Notify test notification"
	}
	if ov.Title != "" {
		p["title"] = ov.Title
	}
	if ov.Message != "" {
		p["message"] = ov.Message
	}
	if n.priority != 0 {
		p["priority"] = n.priority
	}
	if ov.Priority != 0 {
		p["priority"] = ov.Priority
	}
	if tags := cfgStringSlice(map[string]any{"t": n.tags}, "t"); len(tags) > 0 {
		p["tags"] = tags
	}
	if len(ov.Tags) > 0 {
		p["tags"] = ov.Tags
	}
	if ov.Markdown {
		p["markdown"] = true
	}
	if len(ov.Actions) > 0 {
		p["actions"] = ov.Actions
	}
	sid := ov.Edit // replacing a notification means publishing under its ID
	if ov.Ref != "" {
		sid = ov.Ref
	}
	for k, v := range map[string]string{"click": ov.Click, "attach": ov.Attach, "filename": ov.Filename,
		"icon": ov.Icon, "delay": ov.Delay, "email": ov.Email, "sequence_id": ntfySID(sid)} {
		if v != "" {
			p[k] = v
		}
	}
	_, err := sendJSON(ctx, n.client, http.MethodPost, root, p, n.authHeaders())
	return err
}

// dismiss clears and deletes the notifications the override names. Best
// effort, except when it is the whole delivery (strict).
func (n *ntfy) dismiss(ctx context.Context, ov ntfyOverride, strict bool) error {
	root, topic := n.rootAndTopic()
	var retry error
	do := func(what, method, path string) {
		_, err := doRequest(ctx, n.client, method, root+url.PathEscape(topic)+path, "", nil, n.authHeaders())
		if err != nil {
			slog.Warn("ntfy: "+what+" failed", "err", err)
			if retry == nil && !isPayloadRefused(err) && !isNotFound(err) {
				retry = fmt.Errorf("ntfy %s: %w", what, err)
			}
		}
	}
	if ov.Clear != "" {
		do("clear", http.MethodPut, "/"+ntfySID(ov.Clear)+"/clear")
	}
	if ov.Delete != "" {
		do("delete", http.MethodDelete, "/"+ntfySID(ov.Delete))
	}
	if strict {
		return retry
	}
	return nil
}

// rootAndTopic splits the topic URL into the server's root URL (ending in /)
// and the topic.
func (n *ntfy) rootAndTopic() (string, string) {
	u, err := url.Parse(n.url)
	if err != nil {
		return n.url, ""
	}
	p := strings.TrimRight(u.Path, "/")
	i := strings.LastIndex(p, "/")
	topic := p[i+1:]
	u.Path, u.RawQuery = p[:i+1], ""
	return u.String(), topic
}

func (n *ntfy) authHeaders() map[string]string {
	if n.token == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + sanitizeHeader(n.token)}
}

// sendPlain publishes the plain message to the topic URL, options as headers.
func (n *ntfy) sendPlain(ctx context.Context, msg models.NotificationMessage) error {
	ev := msg.Event
	title := headline(ev)
	if msg.Test {
		title = "Omni-Notify test notification"
	}
	body := bodyText(ev)

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
