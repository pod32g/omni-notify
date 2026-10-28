package providers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
)

// Callback buttons (the callbacks annotation) make Omni-Notify POST to a URL
// when pressed in Telegram. A button's callback_data holds no URL (Telegram
// allows 64 bytes): it names the stored event and the entry's index in its
// callbacks annotation, signed with a key derived from the bot token and bound
// to the chat, so a modified client cannot invent or move a button. When
// pressed, the action is looked up again in the stored event and checked
// against the provider's current config.
//
// Omni-Notify has no address Telegram can reach, so instead of a webhook it
// long-polls getUpdates (one poller per bot), asking for callback queries only.

// callbackDone is the callback_data of a button that already did its work.
const callbackDone = "c1:done"

// callbackKey derives the HMAC key for callback_data from the bot token.
func callbackKey(token string) []byte {
	sum := sha256.Sum256([]byte("omni-notify telegram callback\x00" + token))
	return sum[:]
}

// signCallback returns callback_data for entry index of the stored event's
// callbacks annotation: "c1:<event>:<index>:<mac>", at most 50 bytes.
func signCallback(key []byte, chatID string, eventRef int64, index int) string {
	body := fmt.Sprintf("c1:%d:%d", eventRef, index)
	return body + ":" + callbackMAC(key, chatID, body)
}

func callbackMAC(key []byte, chatID, body string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(body + "\x00" + chatID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:12])
}

// parseCallback verifies data signed by signCallback for chatID.
func parseCallback(key []byte, chatID, data string) (eventRef int64, index int, ok bool) {
	parts := strings.Split(data, ":")
	if len(parts) != 4 || parts[0] != "c1" {
		return 0, 0, false
	}
	body := strings.Join(parts[:3], ":")
	if !hmac.Equal([]byte(parts[3]), []byte(callbackMAC(key, chatID, body))) {
		return 0, 0, false
	}
	eventRef, err1 := strconv.ParseInt(parts[1], 10, 64)
	index, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || eventRef <= 0 || index < 0 {
		return 0, 0, false
	}
	return eventRef, index, true
}

// CallbackStore is what the callback service reads: the providers (each bot's
// chat and allowlists, read fresh on every press) and the pressed buttons'
// events. *storage.Store satisfies it.
type CallbackStore interface {
	ListProviders(ctx context.Context) ([]models.ProviderConfig, error)
	GetEvent(ctx context.Context, id int64) (models.StoredEvent, error)
}

// TelegramCallbacks polls Telegram for presses of callback buttons and runs
// them, for every bot with an enabled Telegram provider that has
// callback_hosts.
type TelegramCallbacks struct {
	store        CallbackStore
	allowPrivate bool
	log          *slog.Logger

	apiClient  *http.Client // Bot API calls other than getUpdates
	pollClient *http.Client // getUpdates long polls
	actClient  *http.Client // the callback requests

	now        func() time.Time
	rescan     time.Duration // how often the providers are re-read
	pollWait   int           // getUpdates long-poll timeout, seconds
	retryBase  time.Duration // first wait after a failed getUpdates
	retryMax   time.Duration
	actTimeout time.Duration // one callback request
}

// NewTelegramCallbacks returns the callback service; Run starts it.
func NewTelegramCallbacks(store CallbackStore, allowPrivate bool, log *slog.Logger) *TelegramCallbacks {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	const pollWait = 25
	act := NewGuardedClient(0, allowPrivate)
	// A callback is a POST: never follow a redirect (Go would turn it into a GET
	// on another address); a 3xx answer is reported as a failure.
	act.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &TelegramCallbacks{
		store:        store,
		allowPrivate: allowPrivate,
		log:          log,
		apiClient:    NewGuardedClient(15*time.Second, allowPrivate),
		pollClient:   NewGuardedClient((pollWait+15)*time.Second, allowPrivate),
		actClient:    act,
		now:          time.Now,
		rescan:       30 * time.Second,
		pollWait:     pollWait,
		retryBase:    2 * time.Second,
		retryMax:     time.Minute,
		// Omni-Media gives a retry up to 3 minutes (it may search).
		actTimeout: 3*time.Minute + 15*time.Second,
	}
}

// bot is one bot to poll, identified by its API base and token.
type bot struct {
	key     string // apiBase + "\x00" + token
	apiBase string
	token   string
}

// Run polls until ctx is cancelled, re-reading the providers every rescan to
// start a poller for each new bot and stop those no longer configured. It
// returns once every poller has stopped.
func (c *TelegramCallbacks) Run(ctx context.Context) {
	running := map[string]context.CancelFunc{}
	var wg sync.WaitGroup
	defer func() {
		for _, stop := range running {
			stop()
		}
		wg.Wait()
	}()
	ticker := time.NewTicker(c.rescan)
	defer ticker.Stop()
	for {
		if want, err := c.bots(ctx); err != nil {
			if ctx.Err() == nil {
				c.log.Error("telegram callbacks: list providers", "err", err)
			}
		} else {
			for key, stop := range running {
				if _, ok := want[key]; !ok {
					stop()
					delete(running, key)
				}
			}
			for key, b := range want {
				if _, ok := running[key]; ok {
					continue
				}
				pctx, stop := context.WithCancel(ctx)
				running[key] = stop
				wg.Add(1)
				go func() {
					defer wg.Done()
					c.poll(pctx, b)
				}()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// bots returns the bots that have an enabled Telegram provider with
// callback_hosts.
func (c *TelegramCallbacks) bots(ctx context.Context) (map[string]bot, error) {
	provs, err := c.store.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bot{}
	for _, p := range provs {
		if p.Kind != "telegram" || !p.Enabled {
			continue
		}
		s, err := parseTelegramSettings(p.Config, p.Secret, c.allowPrivate)
		if err != nil || len(s.callbackHosts) == 0 {
			continue
		}
		key := s.apiBase + "\x00" + s.token
		out[key] = bot{key: key, apiBase: s.apiBase, token: s.token}
	}
	return out, nil
}

// poll long-polls getUpdates for one bot until ctx is cancelled, handling each
// callback query in its own goroutine.
func (c *TelegramCallbacks) poll(ctx context.Context, b bot) {
	api := tgAPI{client: c.pollClient, base: b.apiBase + "/bot" + b.token}
	var offset int64
	var handlers sync.WaitGroup
	defer func() {
		handlers.Wait()
		c.confirm(b, offset)
	}()
	failures := 0
	for ctx.Err() == nil {
		var updates []tgUpdate
		err := api.call(ctx, "getUpdates", map[string]any{
			"offset":          offset,
			"timeout":         c.pollWait,
			"allowed_updates": []string{"callback_query"},
		}, &updates)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			failures++
			// A 409 means a webhook is set on this bot, or something else polls it.
			c.log.Warn("telegram callbacks: getUpdates failed", "api", b.apiBase, "failures", failures, "err", err)
			wait := c.retryBase << min(failures-1, 10)
			if wait > c.retryMax {
				wait = c.retryMax
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			continue
		}
		failures = 0
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if cq := u.CallbackQuery; cq != nil {
				handlers.Add(1)
				go func() {
					defer handlers.Done()
					c.handle(ctx, b, *cq)
				}()
			}
		}
	}
}

// confirm tells Telegram the updates before offset were handled, so a restart
// does not get them again (they are confirmed only by the next getUpdates).
func (c *TelegramCallbacks) confirm(b bot, offset int64) {
	if offset == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	api := tgAPI{client: c.apiClient, base: b.apiBase + "/bot" + b.token}
	var ignored []tgUpdate
	if err := api.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": 0, "limit": 1,
		"allowed_updates": []string{"callback_query"},
	}, &ignored); err != nil {
		c.log.Warn("telegram callbacks: confirm updates", "err", err)
	}
}

// callbackAction is a pressed button resolved to what it does.
type callbackAction struct {
	label    string
	url      string
	eventRef int64
	settings telegramSettings
}

// handle runs one pressed button: it answers the press at once (Telegram shows
// a spinner until then), then calls the URL; success marks the button done,
// failure is told in a reply to the message.
func (c *TelegramCallbacks) handle(ctx context.Context, b bot, cq tgCallbackQuery) {
	api := tgAPI{client: c.apiClient, base: b.apiBase + "/bot" + b.token}
	if cq.Data == callbackDone {
		_ = c.answer(ctx, api, cq.ID, "Already done.", false)
		return
	}
	act, refusal := c.resolve(ctx, b, cq)
	if refusal != "" {
		c.log.Warn("telegram callbacks: press refused", "reason", refusal,
			"user_id", cq.From.ID, "chat_id", chatIDOf(cq))
		_ = c.answer(ctx, api, cq.ID, refusal, true)
		return
	}
	// A press Telegram no longer lets us answer (Omni-Notify was down when it
	// was made) is not acted on: nobody is waiting for it any more.
	if err := c.answer(ctx, api, cq.ID, act.label+"…", false); err != nil {
		c.log.Warn("telegram callbacks: press not answered, not acting on it",
			"label", act.label, "event_ref", act.eventRef, "err", err)
		return
	}
	start := time.Now()
	err := c.run(ctx, act)
	attrs := []any{"label", act.label, "event_ref", act.eventRef, "user_id", cq.From.ID,
		"duration_ms", time.Since(start).Milliseconds()}
	if err != nil {
		c.log.Warn("telegram callback failed", append(attrs, "err", err)...)
		if ctx.Err() == nil {
			c.reply(ctx, api, cq, fmt.Sprintf("⚠️ %s didn't work: %v", act.label, err))
		}
		return
	}
	c.log.Info("telegram callback", attrs...)
	c.markDone(ctx, api, cq, act.label)
}

// resolve checks a press and looks its action up, returning a refusal for the
// user when it may not run.
func (c *TelegramCallbacks) resolve(ctx context.Context, b bot, cq tgCallbackQuery) (callbackAction, string) {
	const stale = "This button no longer works."
	if cq.Message == nil {
		return callbackAction{}, stale
	}
	s, ok, err := c.settingsFor(ctx, b, cq.Message.Chat)
	if err != nil {
		c.log.Error("telegram callbacks: list providers", "err", err)
		return callbackAction{}, "Omni-Notify could not check this button; try again."
	}
	if !ok {
		return callbackAction{}, "These buttons don't work in this chat."
	}
	if len(s.callbackUsers) > 0 && !s.callbackUsers[cq.From.ID] {
		return callbackAction{}, "You can't use these buttons."
	}
	eventRef, index, ok := parseCallback(callbackKey(s.token), s.chatID, cq.Data)
	if !ok {
		return callbackAction{}, stale
	}
	ev, err := c.store.GetEvent(ctx, eventRef)
	if err != nil {
		return callbackAction{}, stale
	}
	if c.now().Sub(ev.ReceivedAt) > s.callbackTTL {
		return callbackAction{}, "This button has expired."
	}
	links := parseLinks(ev.Annotations["callbacks"])
	if index >= len(links) {
		return callbackAction{}, stale
	}
	l := links[index]
	if !s.callbackHosts.allows(l.URL) || validateHTTPURL(l.URL, c.allowPrivate) != nil {
		return callbackAction{}, "This button's address is not allowed."
	}
	return callbackAction{label: strings.TrimSpace(l.Label), url: strings.TrimSpace(l.URL), eventRef: eventRef, settings: s}, ""
}

// settingsFor finds the enabled Telegram provider of this bot for chat, with
// callback buttons on.
func (c *TelegramCallbacks) settingsFor(ctx context.Context, b bot, chat tgChat) (telegramSettings, bool, error) {
	provs, err := c.store.ListProviders(ctx)
	if err != nil {
		return telegramSettings{}, false, err
	}
	for _, p := range provs {
		if p.Kind != "telegram" || !p.Enabled {
			continue
		}
		s, err := parseTelegramSettings(p.Config, p.Secret, c.allowPrivate)
		if err != nil || len(s.callbackHosts) == 0 || s.apiBase+"\x00"+s.token != b.key {
			continue
		}
		if s.matchesChat(chat) {
			return s, true, nil
		}
	}
	return telegramSettings{}, false, nil
}

// run makes the callback request: a POST with no body and the provider's
// callback_headers. Any 2xx answer is success.
func (c *TelegramCallbacks) run(ctx context.Context, act callbackAction) error {
	ctx, cancel := context.WithTimeout(ctx, c.actTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, act.url, http.NoBody)
	if err != nil {
		return err
	}
	for k, v := range act.settings.callbackHeaders {
		req.Header.Set(k, v)
	}
	resp, err := c.actClient.Do(req)
	if err != nil {
		return withoutURL(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil
	}
	return answerError(resp)
}

// answerError describes a failed callback answer: its {"error": "..."} when
// it has one, else the start of its body.
func answerError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var body struct {
		Error string `json:"error"`
	}
	msg := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &body) == nil && body.Error != "" {
		msg = body.Error
	}
	msg = truncate(msg, 300)
	if msg == "" {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return errors.New(msg)
}

// answer answers a callback query: a toast, or an alert to dismiss.
func (c *TelegramCallbacks) answer(ctx context.Context, api tgAPI, id, text string, alert bool) error {
	return api.call(ctx, "answerCallbackQuery", map[string]any{
		"callback_query_id": id,
		"text":              truncate(text, 200),
		"show_alert":        alert,
	}, nil)
}

// reply posts text as a reply to the pressed button's message.
func (c *TelegramCallbacks) reply(ctx context.Context, api tgAPI, cq tgCallbackQuery, text string) {
	err := api.call(ctx, "sendMessage", map[string]any{
		"chat_id": cq.Message.Chat.ID,
		"text":    truncate(text, telegramTextLimit),
		"reply_parameters": map[string]any{
			"message_id":                  cq.Message.MessageID,
			"allow_sending_without_reply": true,
		},
	}, nil)
	if err != nil {
		c.log.Warn("telegram callbacks: reply", "err", err)
	}
}

// markDone replaces the pressed button with "✓ <label>", keeping the others.
func (c *TelegramCallbacks) markDone(ctx context.Context, api tgAPI, cq tgCallbackQuery, label string) {
	kb := cq.Message.ReplyMarkup
	if kb == nil {
		return
	}
	rows := make([][]tgButton, len(kb.InlineKeyboard))
	for i, row := range kb.InlineKeyboard {
		rows[i] = make([]tgButton, len(row))
		for j, btn := range row {
			if btn.CallbackData == cq.Data {
				btn = tgButton{Text: truncate("✓ "+label, buttonLabelLimit), CallbackData: callbackDone}
			}
			rows[i][j] = btn
		}
	}
	err := api.call(ctx, "editMessageReplyMarkup", map[string]any{
		"chat_id":      cq.Message.Chat.ID,
		"message_id":   cq.Message.MessageID,
		"reply_markup": tgKeyboard{InlineKeyboard: rows},
	}, nil)
	if err != nil {
		c.log.Warn("telegram callbacks: mark button done", "err", err)
	}
}

func chatIDOf(cq tgCallbackQuery) int64 {
	if cq.Message == nil {
		return 0
	}
	return cq.Message.Chat.ID
}
