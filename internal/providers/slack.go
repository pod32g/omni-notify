package providers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/pod32g/omni-notify/internal/models"
)

// slack posts to a Slack incoming webhook using a coloured attachment.
//
// secret: the webhook URL (required).
// config: username (optional), icon_emoji (optional).
type slack struct {
	client    *http.Client
	url       string
	username  string
	iconEmoji string
}

func newSlack(client *http.Client, cfg map[string]any, secret string, allowPrivate bool) (Provider, error) {
	if secret == "" {
		return nil, fmt.Errorf("slack: webhook URL secret is required")
	}
	if err := validateHTTPURL(secret, allowPrivate); err != nil {
		return nil, fmt.Errorf("slack: %w", err)
	}
	return &slack{
		client:    client,
		url:       secret,
		username:  cfgString(cfg, "username"),
		iconEmoji: cfgString(cfg, "icon_emoji"),
	}, nil
}

type slackField struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

type slackAttachment struct {
	Color  string       `json:"color,omitempty"`
	Title  string       `json:"title,omitempty"`
	Text   string       `json:"text,omitempty"`
	Fields []slackField `json:"fields,omitempty"`
	TS     int64        `json:"ts,omitempty"`
}

func (s *slack) Send(ctx context.Context, msg models.NotificationMessage) error {
	ov := overrideOf(msg.Event, "slack", parseSlackOverride)
	plain := s.payload(msg, slackOverride{})
	if isEmptyOverride(ov) {
		return s.post(ctx, plain)
	}
	err := s.post(ctx, s.payload(msg, ov))
	if isPayloadRefused(err) {
		// Refused blocks (invalid_blocks) must not cost the message.
		slog.Warn("slack: sending the plain message instead", "err", err)
		return s.post(ctx, plain)
	}
	return err
}

// payload is the producer's text, blocks and attachments when the override
// has any, else the default coloured attachment; with the override's options.
func (s *slack) payload(msg models.NotificationMessage, ov slackOverride) map[string]any {
	ev := msg.Event
	p := map[string]any{}
	if ov.Text != "" || len(ov.Blocks) > 0 || len(ov.Attachments) > 0 {
		text := ov.Text
		if text == "" {
			text = bodyText(ev)
			if !ev.Message.Custom() {
				text = headline(ev)
			}
		}
		p["text"] = text
		if len(ov.Blocks) > 0 {
			p["blocks"] = ov.Blocks
		}
		if len(ov.Attachments) > 0 {
			p["attachments"] = ov.Attachments
		}
	} else {
		att := slackAttachment{
			Color: slackColor(ev),
			Title: headline(ev),
			Text:  summaryText(ev, "\n"),
			TS:    ev.Timestamp.Unix(),
		}
		for _, kv := range detailFields(ev) {
			att.Fields = append(att.Fields, slackField{Title: kv[0], Value: kv[1], Short: true})
		}
		p["attachments"] = []slackAttachment{att}
		if msg.Test {
			p["text"] = "Omni-Notify test notification"
		}
	}
	set := func(k string, v string) {
		if v != "" {
			p[k] = v
		}
	}
	set("username", s.username)
	set("icon_emoji", s.iconEmoji)
	set("username", ov.Username)
	set("icon_emoji", ov.IconEmoji)
	set("icon_url", ov.IconURL)
	set("thread_ts", ov.ThreadTS)
	for k, v := range map[string]*bool{"mrkdwn": ov.Mrkdwn, "unfurl_links": ov.UnfurlLinks, "unfurl_media": ov.UnfurlMedia} {
		if v != nil {
			p[k] = *v
		}
	}
	return p
}

func (s *slack) post(ctx context.Context, payload map[string]any) error {
	_, err := sendJSON(ctx, s.client, http.MethodPost, s.url, payload, nil)
	return err
}
