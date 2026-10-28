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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/"+method, body)
	if err != nil {
		return fmt.Errorf("telegram %s: build request", method)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, withoutURL(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("telegram %s: read answer: %w", method, withoutURL(err))
	}
	var env tgEnvelope
	decodeErr := json.Unmarshal(raw, &env)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		desc := env.Description
		if decodeErr != nil {
			desc = strings.TrimSpace(string(raw[:min(len(raw), 512)]))
		}
		return &tgError{Method: method, Status: resp.StatusCode, Description: desc}
	}
	if result == nil {
		return nil
	}
	if decodeErr != nil {
		return fmt.Errorf("telegram %s: decode answer: %w", method, decodeErr)
	}
	if !env.OK {
		return &tgError{Method: method, Status: resp.StatusCode, Description: env.Description}
	}
	if err := json.Unmarshal(env.Result, result); err != nil {
		return fmt.Errorf("telegram %s: decode result: %w", method, err)
	}
	return nil
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

// tgButton is a link button (URL) or a callback button (CallbackData).
type tgButton struct {
	Text         string `json:"text"`
	URL          string `json:"url,omitempty"`
	CallbackData string `json:"callback_data,omitempty"`
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
