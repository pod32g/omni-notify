package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/pod32g/omni-notify/internal/models"
)

// withMessage returns testEvent with a message: text, and the telegram
// override given as JSON (none when empty).
func withMessage(text, telegram string) models.Event {
	ev := testEvent()
	ev.Fingerprint = "fp-1"
	ev.Message = &models.Message{Text: text}
	if telegram != "" {
		ev.Message.Channels = map[string]json.RawMessage{"telegram": json.RawMessage(telegram)}
	}
	return ev
}

// sendTo sends ev as stored event ref through p.
func sendTo(t *testing.T, p *telegram, ref int64, ev models.Event) {
	t.Helper()
	if err := p.Send(context.Background(), models.NotificationMessage{Event: ev, EventRef: ref}); err != nil {
		t.Fatal(err)
	}
}

// memMessages is an in-memory MessageStore.
type memMessages struct {
	mu   sync.Mutex
	rows map[string]models.SentMessage
}

func newMemMessages() *memMessages { return &memMessages{rows: map[string]models.SentMessage{}} }

func memKey(channel, account, chat, ref string) string {
	return channel + "|" + account + "|" + chat + "|" + ref
}

func (m *memMessages) PutSentMessage(_ context.Context, s models.SentMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[memKey(s.Channel, s.Account, s.Chat, s.Ref)] = s
	return nil
}

func (m *memMessages) GetSentMessage(_ context.Context, channel, account, chat, ref string) (models.SentMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.rows[memKey(channel, account, chat, ref)]
	if !ok {
		return models.SentMessage{}, errNotFound
	}
	return s, nil
}

func (m *memMessages) DeleteSentMessage(_ context.Context, channel, account, chat, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, memKey(channel, account, chat, ref))
	return nil
}

// newRefTelegram is a provider with a message store, on a fake Telegram whose
// sends answer with increasing message IDs from 41.
func newRefTelegram(t *testing.T, cfg map[string]any) (*fakeTelegram, *telegram, *memMessages) {
	t.Helper()
	f := newFakeTelegram(t)
	next := int64(40)
	var mu sync.Mutex
	answer := func(tgCall) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		next++
		return http.StatusOK, fmt.Sprintf(`{"ok":true,"result":{"message_id":%d,"chat":{"id":777}}}`, next)
	}
	for _, m := range []string{"sendMessage", "sendPhoto", "sendDocument", "sendRichMessage"} {
		f.answer(m, answer)
	}
	f.answer("sendMediaGroup", func(tgCall) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		next += 2
		return http.StatusOK, fmt.Sprintf(`{"ok":true,"result":[{"message_id":%d},{"message_id":%d}]}`, next-1, next)
	})
	if cfg == nil {
		cfg = map[string]any{}
	}
	cfg["chat_id"] = "777"
	cfg["api_base"] = f.srv.URL
	store := newMemMessages()
	p, err := newTelegram(f.srv.Client(), cfg, "123456:secret", true, store)
	if err != nil {
		t.Fatal(err)
	}
	return f, p.(*telegram), store
}

func TestValidateMessage(t *testing.T) {
	good := `{
		"text": "<b>Recording failed</b>", "parse_mode": "html",
		"buttons": [[{"text":"Open","url":"https://fansly.com/x","style":"primary"},{"text":"Copy ID","copy_text":"6642"}],
		            [{"text":"Retry","callback_url":"https://api.example/retry","style":"success"},{"text":"Done","disabled":true}]],
		"media": [{"type":"photo","url":"https://img.example/a.jpg","has_spoiler":true},{"type":"video","url":"AgACAgIAAxkB"}],
		"disable_notification": true, "protect_content": true, "message_effect": "fire",
		"link_preview_options": {"is_disabled": false, "prefer_small_media": true}, "message_thread_id": 12,
		"ref": "rec:1", "reply_to": "rec:0", "pin": true, "unpin": "rec:0", "delete": "rec:00",
		"react": {"to": "rec:0", "emoji": "👍"}
	}`
	if err := ValidateMessage(&models.Message{Text: "x", Channels: map[string]json.RawMessage{"telegram": json.RawMessage(good)}}); err != nil {
		t.Fatalf("valid override refused: %v", err)
	}
	bad := map[string]string{
		"unknown field":              `{"txt": "x"}`,
		"parse_mode":                 `{"parse_mode": "BBCode"}`,
		"entities with parse_mode":   `{"text":"x","parse_mode":"HTML","entities":[]}`,
		"rich with two contents":     `{"rich": {"markdown":"# a","html":"<b>a</b>"}}`,
		"rich and media":             `{"rich": {"markdown":"# a"}, "media":[{"type":"photo","url":"https://a/b.jpg"}]}`,
		"media type":                 `{"media":[{"type":"sticker","url":"x"}]}`,
		"animation in album":         `{"media":[{"type":"animation","url":"x"},{"type":"photo","url":"y"}]}`,
		"documents with photos":      `{"media":[{"type":"document","url":"x"},{"type":"photo","url":"y"}]}`,
		"media url scheme":           `{"media":[{"type":"photo","url":"ftp://a/b.jpg"}]}`,
		"button with two actions":    `{"buttons":[[{"text":"x","url":"https://a","copy_text":"b"}]]}`,
		"button without action":      `{"buttons":[[{"text":"x"}]]}`,
		"button style":               `{"buttons":[[{"text":"x","url":"https://a","style":"pink"}]]}`,
		"empty row":                  `{"buttons":[[]]}`,
		"effect":                     `{"message_effect":"sparkles"}`,
		"send false with nothing":    `{"send": false}`,
		"send false with edit":       `{"send": false, "edit": "a", "unpin": "a"}`,
		"react without emoji":        `{"react": {"to": "a"}}`,
		"link_preview_options shape": `{"link_preview_options": true}`,
	}
	for name, raw := range bad {
		err := ValidateMessage(&models.Message{Channels: map[string]json.RawMessage{"telegram": json.RawMessage(raw)}})
		if err == nil || !strings.HasPrefix(err.Error(), "message.telegram: ") {
			t.Errorf("%s: got %v, want a message.telegram error", name, err)
		}
	}
	if err := ValidateMessage(&models.Message{Channels: map[string]json.RawMessage{"carrier-pigeon": json.RawMessage(`{}`)}}); err == nil {
		t.Error("an override for an unknown channel was accepted")
	}
}

func TestTelegramSendsProducerTextAsWritten(t *testing.T) {
	f := newFakeTelegram(t)
	p := newTestTelegram(t, f, map[string]any{"parse_mode": "HTML"})
	ev := withMessage("❌ Recording failed: @CatinaJacket <ffmpeg>", "")
	ev.Labels = map[string]string{"stream_id": "664280933146308609"}
	sendTo(t, p, 7, ev)

	msg := f.callsTo("sendMessage")[0].JSON
	if msg["text"] != "❌ Recording failed: @CatinaJacket <ffmpeg>" {
		t.Fatalf("text %q: want the producer's text and nothing else", msg["text"])
	}
	if _, ok := msg["parse_mode"]; ok {
		t.Fatal("the config's parse_mode applied to the producer's plain text")
	}
}

func TestTelegramOverrideOptionsAndButtons(t *testing.T) {
	f := newFakeTelegram(t)
	p := newTestTelegram(t, f, map[string]any{"message_thread_id": 5, "callback_hosts": []any{"api.example"}})
	sendTo(t, p, 7, withMessage("plain", `{
		"text": "<b>Recording failed</b>", "parse_mode": "HTML",
		"buttons": [[{"text":"Open","url":"https://fansly.com/x","style":"primary"},{"text":"Copy ID","copy_text":"6642"}],
		            [{"text":"Retry","callback_url":"https://api.example/retry","style":"success"},{"text":"Elsewhere","callback_url":"https://evil.example/x"}],
		            [{"text":"Done","disabled":true}]],
		"disable_notification": true, "protect_content": true, "message_effect": "fire",
		"link_preview_options": {"url": "https://fansly.com/x", "prefer_large_media": true}
	}`))

	c := f.callsTo("sendMessage")[0]
	m := c.JSON
	if m["text"] != "<b>Recording failed</b>" || m["parse_mode"] != "HTML" {
		t.Fatalf("text %q parse_mode %v", m["text"], m["parse_mode"])
	}
	if m["disable_notification"] != true || m["protect_content"] != true || m["message_effect_id"] != "5104841245755180586" {
		t.Fatalf("options: %v", m)
	}
	if m["message_thread_id"] != float64(5) {
		t.Fatalf("message_thread_id %v, want the config's 5", m["message_thread_id"])
	}
	if lp, _ := m["link_preview_options"].(map[string]any); lp["url"] != "https://fansly.com/x" {
		t.Fatalf("link_preview_options %v", m["link_preview_options"])
	}
	kb := keyboardOf(t, c)
	if len(kb.InlineKeyboard) != 3 {
		t.Fatalf("rows %+v", kb.InlineKeyboard)
	}
	row0, row1, row2 := kb.InlineKeyboard[0], kb.InlineKeyboard[1], kb.InlineKeyboard[2]
	if row0[0].URL != "https://fansly.com/x" || row0[0].Style != "primary" || row0[1].CopyText == nil || row0[1].CopyText.Text != "6642" {
		t.Fatalf("row 0 %+v", row0)
	}
	if len(row1) != 1 || row1[0].Style != "success" {
		t.Fatalf("row 1 %+v: want Retry only (the other host is not allowed)", row1)
	}
	if v, ref, i, ok := parseCallbackV(callbackKey("tok123"), "777", row1[0].CallbackData); !ok || v != callbackMessage || ref != 7 || i != 0 {
		t.Fatalf("callback_data %q: %v %d %d %v", row1[0].CallbackData, v, ref, i, ok)
	}
	if row2[0].Disabled == nil {
		t.Fatalf("row 2 %+v", row2)
	}
}

func TestTelegramSilentSeverities(t *testing.T) {
	f := newFakeTelegram(t)
	p := newTestTelegram(t, f, map[string]any{"silent_severities": "info, debug"})
	ev := testEvent()
	ev.Severity = models.SeverityInfo
	sendTo(t, p, 1, ev)
	ev.Severity = models.SeverityCritical
	sendTo(t, p, 2, ev)
	ev.Severity = models.SeverityInfo
	ev.Message = &models.Message{Text: "loud", Channels: map[string]json.RawMessage{"telegram": json.RawMessage(`{"disable_notification": false}`)}}
	sendTo(t, p, 3, ev)

	msgs := f.callsTo("sendMessage")
	if msgs[0].JSON["disable_notification"] != true {
		t.Fatal("info was not silent")
	}
	if _, ok := msgs[1].JSON["disable_notification"]; ok {
		t.Fatal("critical was silent")
	}
	if _, ok := msgs[2].JSON["disable_notification"]; ok {
		t.Fatal("the producer's disable_notification false did not win")
	}
	if _, err := newTelegram(http.DefaultClient, map[string]any{"chat_id": "1", "silent_severities": "loud"}, "t", true, nil); err == nil {
		t.Fatal("an unknown severity was accepted")
	}
}

func TestTelegramBrokenMarkupGoesOutAsPlainText(t *testing.T) {
	f := newFakeTelegram(t)
	f.answer("sendMessage", func(c tgCall) (int, string) {
		if c.JSON["parse_mode"] != nil {
			return http.StatusBadRequest, `{"ok":false,"description":"Bad Request: can't parse entities: Unclosed start tag at byte offset 0"}`
		}
		return http.StatusOK, `{"ok":true,"result":{"message_id":1}}`
	})
	p := newTestTelegram(t, f, nil)
	sendTo(t, p, 1, withMessage("", `{"text":"<b>Disk &amp; CPU","parse_mode":"HTML","buttons":[[{"text":"Open","url":"https://a.example"}]]}`))

	msgs := f.callsTo("sendMessage")
	if len(msgs) != 2 {
		t.Fatalf("calls %v", f.methods())
	}
	if msgs[1].JSON["text"] != "Disk & CPU" {
		t.Fatalf("plain text %q", msgs[1].JSON["text"])
	}
	if keyboardOf(t, msgs[1]) == nil {
		t.Fatal("a formatting error cost the buttons")
	}
}

func TestTelegramRichMessageAndFallback(t *testing.T) {
	f := newFakeTelegram(t)
	p := newTestTelegram(t, f, nil)
	rich := `{"rich": {"markdown": "# Nightly\n| a | b |\n|---|---|\n| 1 | 2 |"}, "buttons": [[{"text":"Open","url":"https://a.example"}]]}`
	sendTo(t, p, 1, withMessage("Nightly: 1, 2", rich))
	r := f.callsTo("sendRichMessage")
	if len(r) != 1 || len(f.callsTo("sendMessage")) != 0 {
		t.Fatalf("calls %v", f.methods())
	}
	if rm, _ := r[0].JSON["rich_message"].(map[string]any); !strings.HasPrefix(fmt.Sprint(rm["markdown"]), "# Nightly") {
		t.Fatalf("rich_message %v", r[0].JSON["rich_message"])
	}
	if keyboardOf(t, r[0]) == nil {
		t.Fatal("rich message lost its buttons")
	}

	// A Bot API server without rich messages: the text goes out instead.
	f2 := newFakeTelegram(t)
	f2.answer("sendRichMessage", func(tgCall) (int, string) {
		return http.StatusNotFound, `{"ok":false,"description":"Not Found: method not found"}`
	})
	sendTo(t, newTestTelegram(t, f2, nil), 1, withMessage("Nightly: 1, 2", rich))
	m := f2.callsTo("sendMessage")
	if len(m) != 1 || m[0].JSON["text"] != "Nightly: 1, 2" || keyboardOf(t, m[0]) == nil {
		t.Fatalf("fallback: calls %v", f2.methods())
	}
}

func TestTelegramMediaByURLAndUpload(t *testing.T) {
	img := testPNG(t)
	imgSrv, hits := imageServer(t, "image/png", img)
	docSrv, _ := imageServer(t, "application/pdf", []byte("%PDF-1.4 report"))

	// Not on media_hosts: Telegram fetches the URL itself.
	f := newFakeTelegram(t)
	p := newTestTelegram(t, f, nil)
	sendTo(t, p, 1, withMessage("poster", `{"media":[{"type":"photo","url":"https://img.example/p.jpg","has_spoiler":true}],"show_caption_above_media":true}`))
	c := f.callsTo("sendPhoto")[0].JSON
	if c["photo"] != "https://img.example/p.jpg" || c["caption"] != "poster" || c["has_spoiler"] != true || c["show_caption_above_media"] != true {
		t.Fatalf("sendPhoto %v", c)
	}

	// On media_hosts: fetched and uploaded, the document under its own name.
	f = newFakeTelegram(t)
	p = newTestTelegram(t, f, map[string]any{"media_hosts": []any{"127.0.0.1"}})
	sendTo(t, p, 1, withMessage("poster", fmt.Sprintf(`{"media":[{"type":"photo","url":%q}]}`, imgSrv.URL+"/p.png")))
	sendTo(t, p, 2, withMessage("report", fmt.Sprintf(`{"media":[{"type":"document","url":%q}]}`, docSrv.URL+"/reports/nightly.pdf")))
	if *hits != 1 || string(f.callsTo("sendPhoto")[0].Files["photo"]) != string(img) {
		t.Fatalf("photo not uploaded (hits %d)", *hits)
	}
	doc := f.callsTo("sendDocument")[0]
	if doc.FileName != "nightly.pdf" || doc.Form["caption"] != "report" {
		t.Fatalf("document %q caption %q", doc.FileName, doc.Form["caption"])
	}
}

func TestTelegramLongCaptionFollowsAsReply(t *testing.T) {
	f, p, _ := newRefTelegram(t, nil)
	long := strings.Repeat("a", telegramCaptionLimit+1)
	sendTo(t, p, 1, withMessage(long, `{"media":[{"type":"photo","url":"https://img.example/p.jpg"}],"buttons":[[{"text":"Open","url":"https://a.example"}]]}`))

	photo := f.callsTo("sendPhoto")[0]
	if _, ok := photo.JSON["caption"]; ok || keyboardOf(t, photo) != nil {
		t.Fatalf("photo %v: want no caption or buttons", photo.JSON)
	}
	text := f.callsTo("sendMessage")[0]
	if text.JSON["text"] != long || keyboardOf(t, text) == nil {
		t.Fatal("text with the buttons did not follow")
	}
	if rp, _ := text.JSON["reply_parameters"].(map[string]any); rp["message_id"] != float64(41) {
		t.Fatalf("reply_parameters %v, want a reply to the photo", text.JSON["reply_parameters"])
	}
}

func TestTelegramAlbum(t *testing.T) {
	media := `[{"type":"photo","url":"https://img.example/1.jpg"},{"type":"photo","url":"https://img.example/2.jpg","caption":"second"},{"type":"video","url":"https://img.example/3.mp4"}]`
	f, p, _ := newRefTelegram(t, nil)
	sendTo(t, p, 1, withMessage("three", `{"media":`+media+`}`))
	album := f.callsTo("sendMediaGroup")[0].JSON["media"].([]any)
	if len(album) != 3 || album[0].(map[string]any)["caption"] != "three" || album[1].(map[string]any)["caption"] != "second" {
		t.Fatalf("album %v", album)
	}
	if len(f.callsTo("sendMessage")) != 0 {
		t.Fatal("text sent apart though it fit the caption")
	}

	// With buttons (an album can't have any), the text and buttons follow.
	f, p, _ = newRefTelegram(t, nil)
	sendTo(t, p, 1, withMessage("three", `{"media":`+media+`,"buttons":[[{"text":"Open","url":"https://a.example"}]]}`))
	if _, ok := f.callsTo("sendMediaGroup")[0].JSON["media"].([]any)[0].(map[string]any)["caption"]; ok {
		t.Fatal("caption on the album though the text follows")
	}
	text := f.callsTo("sendMessage")[0]
	if rp, _ := text.JSON["reply_parameters"].(map[string]any); rp["message_id"] != float64(41) || keyboardOf(t, text) == nil {
		t.Fatalf("follow-up %v", text.JSON)
	}
}

func TestTelegramRefsEditReplyPinReact(t *testing.T) {
	f, p, store := newRefTelegram(t, nil)

	// Firing: remembered under its ref, pinned.
	sendTo(t, p, 1, withMessage("🔴 Pi-hole down", `{"ref":"pihole","pin":true}`))
	if got := f.callsTo("pinChatMessage"); len(got) != 1 || got[0].JSON["message_id"] != float64(41) {
		t.Fatalf("pin: calls %v", f.methods())
	}
	if m, err := store.GetSentMessage(context.Background(), "telegram", "123456", "777", "pihole"); err != nil || m.MessageID != "41" || m.Kind != kindText {
		t.Fatalf("remembered %+v %v", m, err)
	}

	// A reply to it.
	sendTo(t, p, 2, withMessage("still down", `{"reply_to":"pihole","ref":"pihole-2"}`))
	if rp, _ := f.callsTo("sendMessage")[1].JSON["reply_parameters"].(map[string]any); rp["message_id"] != float64(41) {
		t.Fatalf("reply_parameters %v", rp)
	}

	// Resolved: the firing message is edited, unpinned and reacted to.
	sendTo(t, p, 3, withMessage("✅ Pi-hole back", `{"edit":"pihole","unpin":"pihole","react":{"emoji":"👍"},"buttons":[[{"text":"Logs","url":"https://a.example"}]]}`))
	edit := f.callsTo("editMessageText")
	if len(edit) != 1 || edit[0].JSON["message_id"] != float64(41) || edit[0].JSON["text"] != "✅ Pi-hole back" || keyboardOf(t, edit[0]) == nil {
		t.Fatalf("edit: calls %v", f.methods())
	}
	if len(f.callsTo("sendMessage")) != 2 {
		t.Fatal("the edit also sent a new message")
	}
	if u := f.callsTo("unpinChatMessage"); len(u) != 1 || u[0].JSON["message_id"] != float64(41) {
		t.Fatalf("unpin: calls %v", f.methods())
	}
	r := f.callsTo("setMessageReaction")
	if len(r) != 1 || r[0].JSON["message_id"] != float64(41) {
		t.Fatalf("react: calls %v", f.methods())
	}
	if react := r[0].JSON["reaction"].([]any)[0].(map[string]any); react["type"] != "emoji" || react["emoji"] != "👍" {
		t.Fatalf("reaction %v", react)
	}
}

func TestTelegramEditFallbacks(t *testing.T) {
	f, p, _ := newRefTelegram(t, nil)
	sendTo(t, p, 1, withMessage("poster", `{"ref":"ep","media":[{"type":"photo","url":"https://img.example/p.jpg"}]}`))

	// A media message is edited by its caption.
	sendTo(t, p, 2, withMessage("poster, downloaded", `{"edit":"ep"}`))
	if c := f.callsTo("editMessageCaption"); len(c) != 1 || c[0].JSON["caption"] != "poster, downloaded" {
		t.Fatalf("calls %v", f.methods())
	}

	// Unchanged is fine.
	f.answer("editMessageCaption", func(tgCall) (int, string) {
		return http.StatusBadRequest, `{"ok":false,"description":"Bad Request: message is not modified"}`
	})
	sendTo(t, p, 3, withMessage("poster, downloaded", `{"edit":"ep"}`))
	if len(f.callsTo("sendMessage")) != 0 {
		t.Fatal("an unmodified edit sent a new message")
	}

	// A message Telegram won't edit any more: a new one is sent and remembered.
	f.answer("editMessageCaption", func(tgCall) (int, string) {
		return http.StatusBadRequest, `{"ok":false,"description":"Bad Request: message to edit not found"}`
	})
	sendTo(t, p, 4, withMessage("poster, gone", `{"edit":"ep","ref":"ep"}`))
	if len(f.callsTo("sendMessage")) != 1 {
		t.Fatalf("calls %v: want a new message", f.methods())
	}
	// An unknown ref is sent as new, and the new message takes the ref over.
	sendTo(t, p, 5, withMessage("other", `{"edit":"never-sent"}`))
	if len(f.callsTo("sendMessage")) != 2 {
		t.Fatalf("calls %v", f.methods())
	}
	sendTo(t, p, 6, withMessage("other, finished", `{"edit":"never-sent"}`))
	if len(f.callsTo("sendMessage")) != 2 || len(f.callsTo("editMessageText")) != 1 {
		t.Fatalf("calls %v: want the replacement edited", f.methods())
	}
}

func TestTelegramDefaultRefIsTheFingerprint(t *testing.T) {
	f, p, _ := newRefTelegram(t, nil)
	sendTo(t, p, 1, withMessage("first", ""))
	sendTo(t, p, 2, withMessage("second", `{"reply_to":"fp-1"}`))
	if rp, _ := f.callsTo("sendMessage")[1].JSON["reply_parameters"].(map[string]any); rp["message_id"] != float64(41) {
		t.Fatalf("reply_parameters %v", rp)
	}
}

func TestTelegramActionsOnly(t *testing.T) {
	f, p, store := newRefTelegram(t, nil)
	sendTo(t, p, 1, withMessage("recording started", `{"ref":"rec"}`))
	sendTo(t, p, 2, withMessage("", `{"send":false,"react":{"to":"rec","emoji":"🔥"},"delete":"rec"}`))
	if got := strings.Join(f.methods(), ","); got != "sendMessage,setMessageReaction,deleteMessage" {
		t.Fatalf("calls %s", got)
	}
	if _, err := store.GetSentMessage(context.Background(), "telegram", "123456", "777", "rec"); err == nil {
		t.Fatal("a deleted message is still remembered")
	}

	// With send false the actions are the delivery: an outage is retried, a
	// refusal (nothing left to do) is not.
	sendTo(t, p, 3, withMessage("again", `{"ref":"rec"}`))
	f.answer("unpinChatMessage", func(tgCall) (int, string) {
		return http.StatusBadGateway, `{"ok":false,"description":"Bad Gateway"}`
	})
	err := p.Send(context.Background(), models.NotificationMessage{Event: withMessage("", `{"send":false,"unpin":"rec"}`), EventRef: 4})
	if err == nil {
		t.Fatal("an outage during send false was not an error")
	}
	f.answer("unpinChatMessage", func(tgCall) (int, string) {
		return http.StatusBadRequest, `{"ok":false,"description":"Bad Request: message not found"}`
	})
	if err := p.Send(context.Background(), models.NotificationMessage{Event: withMessage("", `{"send":false,"unpin":"rec"}`), EventRef: 5}); err != nil {
		t.Fatalf("a refusal was retried: %v", err)
	}
}

func TestTelegramTestSendsRememberNothing(t *testing.T) {
	_, p, store := newRefTelegram(t, nil)
	if err := p.Send(context.Background(), models.NotificationMessage{Event: withMessage("x", `{"ref":"t"}`), Test: true}); err != nil {
		t.Fatal(err)
	}
	if len(store.rows) != 0 {
		t.Fatalf("test send remembered %v", store.rows)
	}
}

func TestCallbackPressOfMessageButton(t *testing.T) {
	fx := newCallbackFixture(t, nil)
	ev := fx.store.events[42]
	ev.Message = &models.Message{Text: "x", Channels: map[string]json.RawMessage{"telegram": json.RawMessage(fmt.Sprintf(
		`{"buttons":[[{"text":"Open","url":"https://a.example"},{"text":"Restart","callback_url":%q}],[{"text":"Stop","callback_url":%q}]]}`,
		fx.target.srv.URL+"/restart", fx.target.srv.URL+"/stop"))}}
	fx.store.events[42] = ev

	cq := fx.press(fixtureChat, 1, 0)
	cq.Data = signCallbackV(callbackKey(fixtureToken), "777", callbackMessage, 42, 1)
	cq.Message.ReplyMarkup.InlineKeyboard[0][0].CallbackData = cq.Data
	fx.svc.handle(context.Background(), fx.bot, cq)

	reqs := fx.target.got()
	if len(reqs) != 1 || reqs[0].URL.Path != "/stop" {
		t.Fatalf("target got %v", reqs)
	}
	if text, _ := fx.answerOf(t); text != "Stop…" {
		t.Fatalf("answer %q", text)
	}
	// The same index as the annotation's version names another button.
	if _, _, ok := parseCallback(callbackKey(fixtureToken), "777", cq.Data); ok {
		t.Fatal("a message button's data parsed as an annotation button's")
	}
}

func TestTruncateUnits(t *testing.T) {
	if got := truncateUnits("abc", 3); got != "abc" {
		t.Fatalf("%q", got)
	}
	// 🔥 is two UTF-16 units: four of them are 8, cut to 5 is two and "…".
	if got := truncateUnits("🔥🔥🔥🔥", 5); got != "🔥🔥…" {
		t.Fatalf("%q", got)
	}
	long := strings.Repeat("🔥", telegramTextLimit)
	if n := visibleLen(truncateUnits(long, telegramTextLimit), ""); n > telegramTextLimit {
		t.Fatalf("cut to %d units", n)
	}
}
