package providers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
)

// telegram sends messages through the Telegram Bot API.
//
// secret: the bot token (required).
// config:
//   - chat_id:           target chat ID (required).
//   - parse_mode:        optional formatting mode ("Markdown" or "HTML"); omitted when empty.
//   - api_base:          optional API base URL (default "https://api.telegram.org").
//   - image_hosts:       hosts an event's image_url may be fetched from, to send it
//     as a photo; empty (the default) sends text only.
//   - image_max_bytes:   largest image fetched (default 5 MiB, at most 10 MiB).
//   - callback_hosts:    hosts an event's callbacks may call; empty (the default)
//     renders no callback buttons and polls nothing.
//   - callback_headers:  headers sent with every callback request.
//   - callback_user_ids: Telegram user IDs who may press callback buttons
//     (default: anyone in the chat).
//   - callback_ttl:      how long a callback button works after its event
//     arrived (default "168h").
//
// Three annotations are rendering hints, never listed in the text:
//   - image_url: the message goes out as a photo (sendPhoto) with the text as
//     its caption, when the URL's host is in image_hosts. Omni-Notify fetches
//     the image itself; any failure falls back to a text message.
//   - actions:   a JSON list of {"label","url"}, rendered as link buttons.
//   - callbacks: a JSON list of {"label","url"}, rendered as buttons that make
//     Omni-Notify POST to the URL (see telegram_callbacks.go).
//
// The request URL is api_base + "/bot" + <token> + "/<method>".
type telegram struct {
	api          tgAPI
	client       *http.Client
	allowPrivate bool
	settings     telegramSettings
	callbackKey  []byte
}

// Telegram's limits for a message's text and a photo's caption.
const (
	telegramTextLimit    = 4096
	telegramCaptionLimit = 1024
)

// telegramSettings is a Telegram provider's parsed config, shared by the
// provider and the callback poller.
type telegramSettings struct {
	apiBase         string
	token           string
	chatID          string
	parseMode       string
	imageHosts      hostSet
	imageMaxBytes   int64
	callbackHosts   hostSet
	callbackHeaders map[string]string
	callbackUsers   map[int64]bool
	callbackTTL     time.Duration
}

const (
	defaultImageMaxBytes = 5 << 20
	maxImageBytes        = 10 << 20 // Telegram's limit for a photo
	defaultCallbackTTL   = 7 * 24 * time.Hour
)

func parseTelegramSettings(cfg map[string]any, secret string, allowPrivate bool) (telegramSettings, error) {
	if secret == "" {
		return telegramSettings{}, fmt.Errorf("telegram: bot token secret is required")
	}
	chatID := cfgString(cfg, "chat_id")
	if chatID == "" {
		return telegramSettings{}, fmt.Errorf("telegram: chat_id is required")
	}
	apiBase := strings.TrimRight(cfgStringDefault(cfg, "api_base", "https://api.telegram.org"), "/")
	if err := validateHTTPURL(apiBase, allowPrivate); err != nil {
		return telegramSettings{}, fmt.Errorf("telegram: %w", err)
	}
	s := telegramSettings{
		apiBase:         apiBase,
		token:           secret,
		chatID:          chatID,
		parseMode:       cfgString(cfg, "parse_mode"),
		imageHosts:      newHostSet(cfgStringSlice(cfg, "image_hosts")),
		imageMaxBytes:   int64(cfgInt(cfg, "image_max_bytes", defaultImageMaxBytes)),
		callbackHosts:   newHostSet(cfgStringSlice(cfg, "callback_hosts")),
		callbackHeaders: cfgStringMap(cfg, "callback_headers"),
	}
	if s.imageMaxBytes <= 0 || s.imageMaxBytes > maxImageBytes {
		return telegramSettings{}, fmt.Errorf("telegram: image_max_bytes must be between 1 and %d", maxImageBytes)
	}
	users, err := cfgInt64Set(cfg, "callback_user_ids")
	if err != nil {
		return telegramSettings{}, fmt.Errorf("telegram: callback_user_ids: %w", err)
	}
	s.callbackUsers = users
	if s.callbackTTL, err = cfgDuration(cfg, "callback_ttl", defaultCallbackTTL); err != nil {
		return telegramSettings{}, fmt.Errorf("telegram: callback_ttl: %w", err)
	}
	return s, nil
}

// botBase is the Bot API URL prefix for this provider's bot. It carries the
// token: never log it or put it in an error.
func (s telegramSettings) botBase() string { return s.apiBase + "/bot" + s.token }

// matchesChat reports whether chat is this provider's configured chat, given
// either as a numeric ID or as "@username".
func (s telegramSettings) matchesChat(chat tgChat) bool {
	if strconv.FormatInt(chat.ID, 10) == s.chatID {
		return true
	}
	return chat.Username != "" && strings.EqualFold("@"+chat.Username, s.chatID)
}

func newTelegram(client *http.Client, cfg map[string]any, secret string, allowPrivate bool) (Provider, error) {
	s, err := parseTelegramSettings(cfg, secret, allowPrivate)
	if err != nil {
		return nil, err
	}
	return &telegram{
		api:          tgAPI{client: client, base: s.botBase()},
		client:       client,
		allowPrivate: allowPrivate,
		settings:     s,
		callbackKey:  callbackKey(secret),
	}, nil
}

type telegramPayload struct {
	ChatID                string      `json:"chat_id"`
	Text                  string      `json:"text"`
	DisableWebPagePreview bool        `json:"disable_web_page_preview"`
	ParseMode             string      `json:"parse_mode,omitempty"`
	ReplyMarkup           *tgKeyboard `json:"reply_markup,omitempty"`
}

func (t *telegram) Send(ctx context.Context, msg models.NotificationMessage) error {
	ev := msg.Event
	text := subjectLine(ev) + "\n\n" + plainBody(withoutRenderHints(ev))
	markup := t.keyboard(msg)
	if raw := ev.Annotations["image_url"]; raw != "" && len(t.settings.imageHosts) > 0 {
		err := t.sendPhoto(ctx, raw, truncate(text, telegramCaptionLimit), markup)
		if err == nil {
			return nil
		}
		slog.Warn("telegram: sending without the photo", "event_id", ev.EventID, "err", err)
	}
	return t.sendMessage(ctx, truncate(text, telegramTextLimit), markup)
}

// sendMessage sends text. When Telegram refuses a message with buttons (a link
// it will not open, say), the message goes out again without them: a button
// must never cost the message.
func (t *telegram) sendMessage(ctx context.Context, text string, markup *tgKeyboard) error {
	payload := telegramPayload{
		ChatID:                t.settings.chatID,
		Text:                  text,
		DisableWebPagePreview: true,
		ParseMode:             t.settings.parseMode,
		ReplyMarkup:           markup,
	}
	err := t.api.call(ctx, "sendMessage", payload, nil)
	var refused *tgError
	if markup != nil && errors.As(err, &refused) && refused.Status == http.StatusBadRequest {
		slog.Warn("telegram: sending without buttons", "err", err)
		payload.ReplyMarkup = nil
		return t.api.call(ctx, "sendMessage", payload, nil)
	}
	return err
}

// renderHints are the annotations Telegram renders (a photo, buttons) rather
// than lists as text.
var renderHints = []string{"image_url", "actions", "callbacks"}

// withoutRenderHints returns ev without the annotations rendered as a photo or
// buttons; the caller's map is left alone.
func withoutRenderHints(ev models.Event) models.Event {
	if len(ev.Annotations) == 0 {
		return ev
	}
	ann := make(map[string]string, len(ev.Annotations))
	for k, v := range ev.Annotations {
		ann[k] = v
	}
	for _, k := range renderHints {
		delete(ann, k)
	}
	ev.Annotations = ann
	return ev
}

// hostSet is an allowlist of hostnames, matched case-insensitively and
// regardless of port.
type hostSet map[string]bool

func newHostSet(hosts []string) hostSet {
	set := hostSet{}
	for _, h := range hosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			set[h] = true
		}
	}
	return set
}

// allows reports whether raw is an http(s) URL, without credentials, on an
// allowed host.
func (s hostSet) allows(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && s.allowsURL(u)
}

func (s hostSet) allowsURL(u *url.URL) bool {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return false
	}
	return s[strings.ToLower(u.Hostname())]
}

// cfgInt64Set returns a set of integer IDs given as a list of numbers or
// strings, or as a comma-separated string.
func cfgInt64Set(cfg map[string]any, key string) (map[int64]bool, error) {
	var items []any
	switch v := cfg[key].(type) {
	case nil:
		return nil, nil
	case []any:
		items = v
	case []int:
		for _, n := range v {
			items = append(items, n)
		}
	case []int64:
		for _, n := range v {
			items = append(items, n)
		}
	case []string:
		for _, s := range v {
			items = append(items, s)
		}
	default:
		for _, s := range cfgStringSlice(cfg, key) {
			items = append(items, s)
		}
	}
	set := map[int64]bool{}
	for _, item := range items {
		switch n := item.(type) {
		case int:
			set[int64(n)] = true
		case int64:
			set[n] = true
		case float64:
			if n != float64(int64(n)) {
				return nil, fmt.Errorf("%v is not a whole number", n)
			}
			set[int64(n)] = true
		case string:
			id, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%q is not a number", n)
			}
			set[id] = true
		default:
			return nil, fmt.Errorf("unsupported value %v", item)
		}
	}
	return set, nil
}

// cfgDuration returns a duration given as a string ("168h") or a number of
// seconds, or def when missing or empty.
func cfgDuration(cfg map[string]any, key string, def time.Duration) (time.Duration, error) {
	var d time.Duration
	switch v := cfg[key].(type) {
	case nil:
		return def, nil
	case string:
		if v == "" {
			return def, nil
		}
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return 0, err
		}
		d = parsed
	case float64:
		d = time.Duration(v * float64(time.Second))
	case int:
		d = time.Duration(v) * time.Second
	case int64:
		d = time.Duration(v) * time.Second
	default:
		return 0, fmt.Errorf("unsupported value %v", v)
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive")
	}
	return d, nil
}
