package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"unicode/utf8"

	"github.com/pod32g/omni-notify/internal/models"
)

// discord posts rich embeds to a Discord incoming webhook.
//
// secret: the webhook URL (required).
// config: username (optional), avatar_url (optional).
type discord struct {
	client    *http.Client
	url       string
	username  string
	avatarURL string
}

func newDiscord(client *http.Client, cfg map[string]any, secret string, allowPrivate bool) (Provider, error) {
	if secret == "" {
		return nil, fmt.Errorf("discord: webhook URL secret is required")
	}
	if err := validateHTTPURL(secret, allowPrivate); err != nil {
		return nil, fmt.Errorf("discord: %w", err)
	}
	return &discord{
		client:    client,
		url:       secret,
		username:  cfgString(cfg, "username"),
		avatarURL: cfgString(cfg, "avatar_url"),
	}, nil
}

type discordEmbed struct {
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	Color       int                 `json:"color,omitempty"`
	Timestamp   string              `json:"timestamp,omitempty"`
	Fields      []discordEmbedField `json:"fields,omitempty"`
}

type discordEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type discordPayload struct {
	Content   string         `json:"content,omitempty"`
	Username  string         `json:"username,omitempty"`
	AvatarURL string         `json:"avatar_url,omitempty"`
	Embeds    []discordEmbed `json:"embeds,omitempty"`
}

func (d *discord) Send(ctx context.Context, msg models.NotificationMessage) error {
	ev := msg.Event
	embed := discordEmbed{
		Title:     truncate(subjectLine(ev), 256),
		Color:     statusColorHex(ev),
		Timestamp: ev.Timestamp.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	desc := ev.Summary
	if ev.Description != "" {
		if desc != "" {
			desc += "\n\n"
		}
		desc += ev.Description
	}
	embed.Description = truncate(desc, 4000)
	for _, kv := range detailFields(ev) {
		embed.Fields = append(embed.Fields, discordEmbedField{
			Name:   truncate(kv[0], 256),
			Value:  truncate(kv[1], 1024),
			Inline: true,
		})
	}

	payload := discordPayload{
		Username:  d.username,
		AvatarURL: d.avatarURL,
		Embeds:    []discordEmbed{embed},
	}
	if msg.Test {
		payload.Content = "Omni-Notify test notification"
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("discord: marshal: %w", err)
	}
	return postBytes(ctx, d.client, http.MethodPost, d.url, "application/json", body, nil)
}

// truncate limits s to max characters (runes), appending an ellipsis when
// shortened. It cuts on rune boundaries so the result is always valid UTF-8
// (Discord/Slack reject invalid UTF-8), and treats max as a character count to
// match the providers' documented character limits.
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	keep := max - 1 // reserve one rune for the ellipsis
	count := 0
	for i := range s {
		if count == keep {
			return s[:i] + "…"
		}
		count++
	}
	return s
}
