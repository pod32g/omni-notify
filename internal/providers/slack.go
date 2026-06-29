package providers

import (
	"context"
	"encoding/json"
	"fmt"
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

type slackPayload struct {
	Text        string            `json:"text,omitempty"`
	Username    string            `json:"username,omitempty"`
	IconEmoji   string            `json:"icon_emoji,omitempty"`
	Attachments []slackAttachment `json:"attachments,omitempty"`
}

func (s *slack) Send(ctx context.Context, msg models.NotificationMessage) error {
	ev := msg.Event
	att := slackAttachment{
		Color: slackColor(ev),
		Title: subjectLine(ev),
		TS:    ev.Timestamp.Unix(),
	}
	text := ev.Summary
	if ev.Description != "" {
		if text != "" {
			text += "\n"
		}
		text += ev.Description
	}
	att.Text = text
	for _, kv := range detailFields(ev) {
		att.Fields = append(att.Fields, slackField{Title: kv[0], Value: kv[1], Short: true})
	}

	payload := slackPayload{
		Username:    s.username,
		IconEmoji:   s.iconEmoji,
		Attachments: []slackAttachment{att},
	}
	if msg.Test {
		payload.Text = "Omni-Notify test notification"
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("slack: marshal: %w", err)
	}
	return postBytes(ctx, s.client, http.MethodPost, s.url, "application/json", body, nil)
}
