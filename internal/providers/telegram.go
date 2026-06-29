package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/pod32g/omni-notify/internal/models"
)

// telegram sends messages through the Telegram Bot API.
//
// secret: the bot token (required).
// config:
//   - chat_id:    target chat ID (required).
//   - parse_mode: optional formatting mode ("Markdown" or "HTML"); omitted when empty.
//   - api_base:   optional API base URL (default "https://api.telegram.org").
//
// The request URL is api_base + "/bot" + <token> + "/sendMessage".
type telegram struct {
	client    *http.Client
	url       string
	chatID    string
	parseMode string
}

func newTelegram(client *http.Client, cfg map[string]any, secret string, allowPrivate bool) (Provider, error) {
	if secret == "" {
		return nil, fmt.Errorf("telegram: bot token secret is required")
	}
	chatID := cfgString(cfg, "chat_id")
	if chatID == "" {
		return nil, fmt.Errorf("telegram: chat_id is required")
	}
	apiBase := cfgStringDefault(cfg, "api_base", "https://api.telegram.org")
	if err := validateHTTPURL(apiBase, allowPrivate); err != nil {
		return nil, fmt.Errorf("telegram: %w", err)
	}
	return &telegram{
		client:    client,
		url:       apiBase + "/bot" + secret + "/sendMessage",
		chatID:    chatID,
		parseMode: cfgString(cfg, "parse_mode"),
	}, nil
}

type telegramPayload struct {
	ChatID                string `json:"chat_id"`
	Text                  string `json:"text"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview"`
	ParseMode             string `json:"parse_mode,omitempty"`
}

func (t *telegram) Send(ctx context.Context, msg models.NotificationMessage) error {
	ev := msg.Event
	text := truncate(subjectLine(ev)+"\n\n"+plainBody(ev), 4096)
	payload := telegramPayload{
		ChatID:                t.chatID,
		Text:                  text,
		DisableWebPagePreview: true,
		ParseMode:             t.parseMode,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("telegram: marshal: %w", err)
	}
	return postBytes(ctx, t.client, http.MethodPost, t.url, "application/json", body, nil)
}
