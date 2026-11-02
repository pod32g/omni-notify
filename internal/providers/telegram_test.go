package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/pod32g/omni-notify/internal/models"
)

func TestNewTelegramSend(t *testing.T) {
	srv, body := captureServer(t, http.StatusOK)
	cfg := map[string]any{
		"chat_id":    "12345",
		"parse_mode": "Markdown",
		"api_base":   srv.URL,
	}
	p, err := newTelegram(srv.Client(), cfg, "bot-token", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		ChatID    string `json:"chat_id"`
		Text      string `json:"text"`
		ParseMode string `json:"parse_mode"`
	}
	if err := json.Unmarshal(*body, &payload); err != nil {
		t.Fatalf("invalid telegram payload: %v\n%s", err, *body)
	}
	if payload.ChatID != "12345" {
		t.Fatalf("unexpected chat_id: %q", payload.ChatID)
	}
	if payload.Text == "" {
		t.Fatal("text is empty")
	}
	if !strings.Contains(payload.Text, "Pi-hole Down") {
		t.Fatalf("text missing subject: %q", payload.Text)
	}
	if payload.ParseMode != "Markdown" {
		t.Fatalf("unexpected parse_mode: %q", payload.ParseMode)
	}
}

func TestNewTelegramRequestPath(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := map[string]any{"chat_id": "9", "api_base": srv.URL}
	p, err := newTelegram(srv.Client(), cfg, "tok123", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/bottok123/sendMessage" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("unexpected method: %q", gotMethod)
	}
}

func TestNewTelegramRequiresFields(t *testing.T) {
	// Missing chat_id.
	if _, err := newTelegram(http.DefaultClient, map[string]any{}, "tok", true, nil); err == nil {
		t.Fatal("expected error for missing chat_id")
	}
	// Missing secret (bot token).
	if _, err := newTelegram(http.DefaultClient, map[string]any{"chat_id": "1"}, "", true, nil); err == nil {
		t.Fatal("expected error for missing bot token")
	}
}

// mediaEvent is an Omni-Media-style arrival with a poster and link buttons.
func mediaEvent(imageURL string) models.Event {
	ev := testEvent()
	ev.Annotations = map[string]string{
		"Dashboard": "https://media.example/#/episode/25",
		"image_url": imageURL,
		"actions":   `[{"label":"Watch","url":"https://jf.example/item/abc"},{"label":"Open in Omni-Media","url":"https://media.example/#/episode/25"}]`,
	}
	return ev
}

func newTestTelegram(t *testing.T, f *fakeTelegram, cfg map[string]any) *telegram {
	t.Helper()
	if cfg == nil {
		cfg = map[string]any{}
	}
	cfg["chat_id"] = "777"
	cfg["api_base"] = f.srv.URL
	p, err := newTelegram(f.srv.Client(), cfg, "tok123", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p.(*telegram)
}

func TestTelegramSendsPhotoWithCaptionAndButtons(t *testing.T) {
	f := newFakeTelegram(t)
	img := testPNG(t)
	imgSrv, _ := imageServer(t, "image/png", img)
	p := newTestTelegram(t, f, map[string]any{"image_hosts": []any{"127.0.0.1"}})

	if err := p.Send(context.Background(), models.NotificationMessage{Event: mediaEvent(imgSrv.URL + "/api/series/91/poster")}); err != nil {
		t.Fatal(err)
	}
	if got := f.methods(); len(got) != 1 || got[0] != "sendPhoto" {
		t.Fatalf("calls = %v, want one sendPhoto", got)
	}
	c := f.callsTo("sendPhoto")[0]
	if c.Token != "tok123" || c.Form["chat_id"] != "777" {
		t.Fatalf("token/chat: %q %q", c.Token, c.Form["chat_id"])
	}
	if !bytes.Equal(c.File, img) || c.FileName != "image.png" {
		t.Fatalf("photo not uploaded as fetched: %d bytes as %q", len(c.File), c.FileName)
	}
	caption := c.Form["caption"]
	if !strings.Contains(caption, "Pi-hole Down") || !strings.Contains(caption, "Dashboard: https://media.example") {
		t.Fatalf("caption missing the text: %q", caption)
	}
	for _, hint := range []string{"image_url", "actions:"} {
		if strings.Contains(caption, hint) {
			t.Fatalf("caption lists render hint %q: %q", hint, caption)
		}
	}
	kb := keyboardOf(t, c)
	if kb == nil || len(kb.InlineKeyboard) != 1 || len(kb.InlineKeyboard[0]) != 2 {
		t.Fatalf("want one row of two link buttons, got %+v", kb)
	}
	if b := kb.InlineKeyboard[0][0]; b.Text != "Watch" || b.URL != "https://jf.example/item/abc" || b.CallbackData != "" {
		t.Fatalf("unexpected first button %+v", b)
	}
}

func TestTelegramPhotoNeedsAnAllowedHost(t *testing.T) {
	f := newFakeTelegram(t)
	imgSrv, hits := imageServer(t, "image/png", testPNG(t))
	for name, cfg := range map[string]map[string]any{
		"no image_hosts":  nil,
		"other host":      {"image_hosts": []any{"media.example"}},
		"comma-separated": {"image_hosts": "media.example, jf.example"},
	} {
		p := newTestTelegram(t, f, cfg)
		if err := p.Send(context.Background(), models.NotificationMessage{Event: mediaEvent(imgSrv.URL + "/p.png")}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if *hits != 0 {
		t.Fatalf("image fetched %d times from a host not allowed", *hits)
	}
	if n := len(f.callsTo("sendPhoto")); n != 0 {
		t.Fatalf("sendPhoto called %d times", n)
	}
	if n := len(f.callsTo("sendMessage")); n != 3 {
		t.Fatalf("sendMessage called %d times, want 3", n)
	}
}

func TestTelegramPhotoFallsBackToText(t *testing.T) {
	png := testPNG(t)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(png)
	}))
	t.Cleanup(other.Close)
	otherURL := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)

	cases := []struct {
		name    string
		handler http.HandlerFunc
		cfg     map[string]any
		refuse  bool   // Telegram refuses the sendPhoto
		wantErr string // why fetching the photo fails
	}{
		{name: "not found", wantErr: "status 404", handler: func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }},
		{name: "not an image", wantErr: "not a JPEG, PNG or WebP", handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png") // the bytes decide, not the header
			_, _ = io.WriteString(w, "<html>sign in</html>")
		}},
		{name: "over the size cap", wantErr: "64-byte limit", cfg: map[string]any{"image_max_bytes": 64}, handler: func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(png)
		}},
		{name: "redirect to a host not allowed", wantErr: `host "localhost" is not allowed`, handler: func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, otherURL+"/p.png", http.StatusFound)
		}},
		{name: "telegram refuses the photo", refuse: true, handler: func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(png)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTelegram(t)
			if tc.refuse {
				f.answer("sendPhoto", func(tgCall) (int, string) {
					return http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: IMAGE_PROCESS_FAILED"}`
				})
			}
			imgSrv := httptest.NewServer(tc.handler)
			t.Cleanup(imgSrv.Close)
			cfg := map[string]any{"image_hosts": []any{"127.0.0.1"}}
			for k, v := range tc.cfg {
				cfg[k] = v
			}
			p := newTestTelegram(t, f, cfg)
			if tc.wantErr != "" {
				_, _, err := p.mediaInput(context.Background(), tgMediaItem{tgMediaSpec: tgMediaSpec{Type: "photo", URL: imgSrv.URL + "/p.png"}}, "photo")
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("mediaInput error %v, want %q", err, tc.wantErr)
				}
			}
			if err := p.Send(context.Background(), models.NotificationMessage{Event: mediaEvent(imgSrv.URL + "/p.png")}); err != nil {
				t.Fatal(err)
			}
			if tc.refuse && len(f.callsTo("sendPhoto")) != 1 {
				t.Fatalf("calls %v, want a sendPhoto first", f.methods())
			}
			msgs := f.callsTo("sendMessage")
			if len(msgs) != 1 {
				t.Fatalf("calls %v, want a sendMessage fallback", f.methods())
			}
			if text, _ := msgs[0].JSON["text"].(string); !strings.Contains(text, "Pi-hole Down") {
				t.Fatalf("fallback text: %q", text)
			}
			if keyboardOf(t, msgs[0]) == nil {
				t.Fatal("fallback lost the buttons")
			}
		})
	}
}

func TestTelegramCaptionLimit(t *testing.T) {
	f := newFakeTelegram(t)
	imgSrv, _ := imageServer(t, "image/png", testPNG(t))
	p := newTestTelegram(t, f, map[string]any{"image_hosts": []any{"127.0.0.1"}})
	ev := mediaEvent(imgSrv.URL + "/p.png")
	ev.Description = strings.Repeat("é", 3000)
	if err := p.Send(context.Background(), models.NotificationMessage{Event: ev}); err != nil {
		t.Fatal(err)
	}
	caption := f.callsTo("sendPhoto")[0].Form["caption"]
	if n := utf8.RuneCountInString(caption); n != telegramCaptionLimit {
		t.Fatalf("caption is %d characters, want %d", n, telegramCaptionLimit)
	}
}

func TestTelegramLinkButtonsSkipBadEntries(t *testing.T) {
	f := newFakeTelegram(t)
	p := newTestTelegram(t, f, nil)
	ev := testEvent()
	ev.Annotations = map[string]string{
		"actions": `[{"label":"Watch","url":"https://jf.example/item/1"},` +
			`{"label":"","url":"https://x.example"},` +
			`{"label":"FTP","url":"ftp://x.example/f"},` +
			`{"label":"Relative","url":"/episode/3"},` +
			`{"label":"Page","url":"http://media.example/#/episode/3"},` +
			`{"label":"Third","url":"https://three.example"}]`,
		"note": "kept",
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: ev}); err != nil {
		t.Fatal(err)
	}
	c := f.callsTo("sendMessage")[0]
	kb := keyboardOf(t, c)
	if kb == nil || len(kb.InlineKeyboard) != 2 || len(kb.InlineKeyboard[0]) != 2 || len(kb.InlineKeyboard[1]) != 1 {
		t.Fatalf("want rows of 2+1 buttons, got %+v", kb)
	}
	if got := kb.InlineKeyboard[0][1].Text; got != "Page" {
		t.Fatalf("second button %q, want Page", got)
	}
	text, _ := c.JSON["text"].(string)
	if strings.Contains(text, "actions:") || !strings.Contains(text, "note: kept") {
		t.Fatalf("text should hide actions and keep other annotations: %q", text)
	}

	// Malformed JSON: no buttons, the message still goes out.
	ev.Annotations = map[string]string{"actions": `[{"label":"Watch",`}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: ev}); err != nil {
		t.Fatal(err)
	}
	if kb := keyboardOf(t, f.callsTo("sendMessage")[1]); kb != nil {
		t.Fatalf("malformed actions gave buttons: %+v", kb)
	}
}

func TestTelegramRefusedButtonsDoNotCostTheMessage(t *testing.T) {
	f := newFakeTelegram(t)
	f.answer("sendMessage", func(c tgCall) (int, string) {
		if c.JSON["reply_markup"] != nil {
			return http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: BUTTON_URL_INVALID"}`
		}
		return http.StatusOK, `{"ok":true,"result":{}}`
	})
	p := newTestTelegram(t, f, nil)
	if err := p.Send(context.Background(), models.NotificationMessage{Event: mediaEvent("")}); err != nil {
		t.Fatal(err)
	}
	msgs := f.callsTo("sendMessage")
	if len(msgs) != 2 || keyboardOf(t, msgs[0]) == nil || keyboardOf(t, msgs[1]) != nil {
		t.Fatalf("want a refused message with buttons then one without, got %d calls", len(msgs))
	}

	// Other refusals are delivery errors (retried by the notifier).
	f.answer("sendMessage", func(tgCall) (int, string) {
		return http.StatusTooManyRequests, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 3"}`
	})
	err := p.Send(context.Background(), models.NotificationMessage{Event: mediaEvent("")})
	if err == nil || !strings.Contains(err.Error(), "Too Many Requests") {
		t.Fatalf("want the 429 as an error, got %v", err)
	}
}

func TestTelegramCallbackButtons(t *testing.T) {
	f := newFakeTelegram(t)
	p := newTestTelegram(t, f, map[string]any{"callback_hosts": []any{"media.example"}})
	ev := mediaEvent("")
	ev.Annotations["callbacks"] = `[{"label":"Retry","url":"https://media.example/api/episodes/5/retry"},` +
		`{"label":"Elsewhere","url":"https://evil.example/api/x"},` +
		`{"label":"Try another release","url":"https://media.example/api/episodes/5/another"}]`

	if err := p.Send(context.Background(), models.NotificationMessage{Event: ev, EventRef: 9_000_000_042}); err != nil {
		t.Fatal(err)
	}
	c := f.callsTo("sendMessage")[0]
	kb := keyboardOf(t, c)
	if kb == nil || len(kb.InlineKeyboard) != 2 {
		t.Fatalf("want a callback row and a link row, got %+v", kb)
	}
	row := kb.InlineKeyboard[0]
	if len(row) != 2 || row[0].Text != "Retry" || row[1].Text != "Try another release" {
		t.Fatalf("callback row %+v", row)
	}
	for i, wantIndex := range []int{0, 2} {
		data := row[i].CallbackData
		if len(data) > 64 || row[i].URL != "" {
			t.Fatalf("button %d: %q (%d bytes)", i, data, len(data))
		}
		ref, index, ok := parseCallback(callbackKey("tok123"), "777", data)
		if !ok || ref != 9_000_000_042 || index != wantIndex {
			t.Fatalf("button %d data %q → %d,%d,%v", i, data, ref, index, ok)
		}
		if _, _, ok := parseCallback(callbackKey("tok123"), "778", data); ok {
			t.Fatal("callback data verified for another chat")
		}
	}
	if text, _ := c.JSON["text"].(string); strings.Contains(text, "callbacks:") {
		t.Fatalf("text lists the callbacks annotation: %q", text)
	}

	// A test send has no stored event: no callback buttons.
	if err := p.Send(context.Background(), models.NotificationMessage{Event: ev}); err != nil {
		t.Fatal(err)
	}
	if kb := keyboardOf(t, f.callsTo("sendMessage")[1]); len(kb.InlineKeyboard) != 1 || kb.InlineKeyboard[0][0].URL == "" {
		t.Fatalf("want link buttons only, got %+v", kb)
	}

	// Without callback_hosts nothing is rendered either.
	plain := newTestTelegram(t, f, nil)
	if err := plain.Send(context.Background(), models.NotificationMessage{Event: ev, EventRef: 42}); err != nil {
		t.Fatal(err)
	}
	if kb := keyboardOf(t, f.callsTo("sendMessage")[2]); len(kb.InlineKeyboard) != 1 {
		t.Fatalf("callback buttons without callback_hosts: %+v", kb)
	}
}

func TestTelegramErrorsHideTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close() // connection refused from now on
	p, err := newTelegram(http.DefaultClient, map[string]any{"chat_id": "1", "api_base": base}, "secret-token-123", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = p.Send(context.Background(), models.NotificationMessage{Event: testEvent()})
	if err == nil || strings.Contains(err.Error(), "secret-token-123") {
		t.Fatalf("want an error without the token, got %v", err)
	}
}

func TestTelegramSettingsValidation(t *testing.T) {
	bad := []map[string]any{
		{"image_max_bytes": 11 << 20},
		{"image_max_bytes": -1},
		{"callback_user_ids": []any{"me"}},
		{"callback_user_ids": []any{1.5}},
		{"callback_ttl": "soon"},
		{"callback_ttl": "-1h"},
	}
	for _, extra := range bad {
		cfg := map[string]any{"chat_id": "1"}
		for k, v := range extra {
			cfg[k] = v
		}
		if _, err := newTelegram(http.DefaultClient, cfg, "tok", true, nil); err == nil {
			t.Errorf("config %v accepted", extra)
		}
	}
	s, err := parseTelegramSettings(map[string]any{
		"chat_id":           "1",
		"callback_user_ids": []any{float64(12345), "678"},
		"callback_ttl":      float64(3600),
		"callback_hosts":    "Media.Example",
	}, "tok", true)
	if err != nil {
		t.Fatal(err)
	}
	if !s.callbackUsers[12345] || !s.callbackUsers[678] || s.callbackTTL != time.Hour {
		t.Fatalf("parsed %+v", s)
	}
	if !s.callbackHosts.allows("https://media.example:8443/x") || s.callbackHosts.allows("https://user:pw@media.example/x") ||
		s.callbackHosts.allows("file://media.example/x") {
		t.Fatal("host allowlist matching is wrong")
	}
}
