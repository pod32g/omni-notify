package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// tgAPI calls Telegram Bot API methods. Its base URL carries the bot token, so
// errors never include the request URL.
type tgAPI struct {
	client *http.Client
	base   string // api_base + "/bot" + token
}

// tgError is a method Telegram answered with an error.
type tgError struct {
	Method      string
	Status      int
	Description string
}

func (e *tgError) Error() string {
	if e.Description == "" {
		return fmt.Sprintf("telegram %s: status %d", e.Method, e.Status)
	}
	return fmt.Sprintf("telegram %s: status %d: %s", e.Method, e.Status, e.Description)
}

type tgEnvelope struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
}

// call posts payload as JSON to method and, when result is not nil, decodes
// the answer's result into it.
func (a tgAPI) call(ctx context.Context, method string, payload, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("telegram %s: marshal: %w", method, err)
	}
	return a.do(ctx, method, "application/json", bytes.NewReader(body), result)
}

// do posts body to method. A 2xx answer is success; its result is decoded only
// when result is not nil (so a bare 200 from a stub still counts as sent).
func (a tgAPI) do(ctx context.Context, method, contentType string, body io.Reader, result any) error {
	env, status, decodeErr, err := a.roundTrip(ctx, method, contentType, body)
	if err != nil || result == nil {
		return err
	}
	if decodeErr != nil {
		return fmt.Errorf("telegram %s: decode answer: %w", method, decodeErr)
	}
	if !env.OK {
		return &tgError{Method: method, Status: status, Description: env.Description}
	}
	if err := json.Unmarshal(env.Result, result); err != nil {
		return fmt.Errorf("telegram %s: decode result: %w", method, err)
	}
	return nil
}

// sent posts a send or edit method and returns the IDs of the messages it
// produced (one, or an album's). Like do, any 2xx answer is success: an answer
// without message IDs gives none rather than an error, since the message went
// out either way.
func (a tgAPI) sent(ctx context.Context, method, contentType string, body io.Reader) ([]int64, error) {
	env, _, decodeErr, err := a.roundTrip(ctx, method, contentType, body)
	if err != nil || decodeErr != nil {
		return nil, err
	}
	var one tgMessage
	if json.Unmarshal(env.Result, &one) == nil && one.MessageID != 0 {
		return []int64{one.MessageID}, nil
	}
	var many []tgMessage
	if json.Unmarshal(env.Result, &many) == nil {
		ids := make([]int64, 0, len(many))
		for _, m := range many {
			ids = append(ids, m.MessageID)
		}
		return ids, nil
	}
	return nil, nil
}

// sentJSON is sent with a JSON payload.
func (a tgAPI) sentJSON(ctx context.Context, method string, payload any) ([]int64, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("telegram %s: marshal: %w", method, err)
	}
	return a.sent(ctx, method, "application/json", bytes.NewReader(body))
}

// roundTrip posts body to method. A non-2xx answer is a *tgError; decodeErr
// says whether a 2xx answer's body was a Bot API envelope.
func (a tgAPI) roundTrip(ctx context.Context, method, contentType string, body io.Reader) (env tgEnvelope, status int, decodeErr, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/"+method, body)
	if err != nil {
		return env, 0, nil, fmt.Errorf("telegram %s: build request", method)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := a.client.Do(req)
	if err != nil {
		return env, 0, nil, fmt.Errorf("telegram %s: %w", method, withoutURL(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return env, resp.StatusCode, nil, fmt.Errorf("telegram %s: read answer: %w", method, withoutURL(err))
	}
	decodeErr = json.Unmarshal(raw, &env)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		desc := env.Description
		if decodeErr != nil {
			desc = strings.TrimSpace(string(raw[:min(len(raw), 512)]))
		}
		return env, resp.StatusCode, decodeErr, &tgError{Method: method, Status: resp.StatusCode, Description: desc}
	}
	return env, resp.StatusCode, decodeErr, nil
}

// isRefused reports whether err is Telegram refusing the request itself (a
// 400), as opposed to a network error, rate limit or outage.
func isRefused(err error) bool {
	var te *tgError
	return errors.As(err, &te) && te.Status == http.StatusBadRequest
}

// refusalSays reports whether err is a 400 whose description contains s
// (case-insensitive).
func refusalSays(err error, s string) bool {
	var te *tgError
	return errors.As(err, &te) && te.Status == http.StatusBadRequest &&
		strings.Contains(strings.ToLower(te.Description), s)
}

// withoutURL drops the request URL from a transport error (*url.Error), which
// for the Bot API contains the token.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// tgKeyboard is an inline keyboard (reply_markup).
type tgKeyboard struct {
	InlineKeyboard [][]tgButton `json:"inline_keyboard"`
}

// tgButton is one inline keyboard button: a link (URL), a callback
// (CallbackData), a copy button (CopyText) or a disabled one (Disabled).
type tgButton struct {
	Text              string      `json:"text"`
	URL               string      `json:"url,omitempty"`
	CallbackData      string      `json:"callback_data,omitempty"`
	CopyText          *tgCopyText `json:"copy_text,omitempty"`
	Disabled          *struct{}   `json:"disabled,omitempty"`
	Style             string      `json:"style,omitempty"`
	IconCustomEmojiID string      `json:"icon_custom_emoji_id,omitempty"`
}

// tgCopyText is a CopyTextButton.
type tgCopyText struct {
	Text string `json:"text"`
}

type tgUpdate struct {
	UpdateID      int64            `json:"update_id"`
	CallbackQuery *tgCallbackQuery `json:"callback_query,omitempty"`
}

type tgCallbackQuery struct {
	ID      string     `json:"id"`
	From    tgUser     `json:"from"`
	Message *tgMessage `json:"message,omitempty"`
	Data    string     `json:"data,omitempty"`
}

type tgUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
}

type tgMessage struct {
	MessageID   int64       `json:"message_id"`
	Chat        tgChat      `json:"chat"`
	ReplyMarkup *tgKeyboard `json:"reply_markup,omitempty"`
}

type tgChat struct {
	ID       int64  `json:"id"`
	Username string `json:"username,omitempty"`
}
