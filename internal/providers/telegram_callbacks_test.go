package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
)

var errNotFound = errors.New("not found")

// fakeCallbackStore holds providers and events in memory.
type fakeCallbackStore struct {
	mu     sync.Mutex
	provs  []models.ProviderConfig
	events map[int64]models.StoredEvent
}

func (s *fakeCallbackStore) ListProviders(context.Context) ([]models.ProviderConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]models.ProviderConfig(nil), s.provs...), nil
}

func (s *fakeCallbackStore) GetEvent(_ context.Context, id int64) (models.StoredEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ev, ok := s.events[id]
	if !ok {
		return models.StoredEvent{}, errNotFound
	}
	return ev, nil
}

// fakeTarget stands in for Omni-Media: it records each callback request and
// answers with status and body.
type fakeTarget struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []*http.Request
	status   int
	body     string
}

func newFakeTarget(t *testing.T) *fakeTarget {
	t.Helper()
	ft := &fakeTarget{status: http.StatusOK, body: `{"id":5}`}
	ft.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ft.mu.Lock()
		ft.requests = append(ft.requests, r)
		status, body := ft.status, ft.body
		ft.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(ft.srv.Close)
	return ft
}

func (ft *fakeTarget) got() []*http.Request {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return append([]*http.Request(nil), ft.requests...)
}

// callbackFixture is a bot with callback buttons for chat 777 and one stored
// event (42) whose first callback retries an episode on the fake target.
type callbackFixture struct {
	tg     *fakeTelegram
	target *fakeTarget
	store  *fakeCallbackStore
	svc    *TelegramCallbacks
	bot    bot
	now    time.Time
}

const (
	fixtureToken = "tok123"
	fixtureChat  = int64(777)
)

func newCallbackFixture(t *testing.T, extra map[string]any) *callbackFixture {
	t.Helper()
	fx := &callbackFixture{tg: newFakeTelegram(t), target: newFakeTarget(t), now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	cfg := map[string]any{
		"chat_id":          "777",
		"api_base":         fx.tg.srv.URL,
		"callback_hosts":   []any{"127.0.0.1"},
		"callback_headers": map[string]any{"X-Media-Request": "1"},
	}
	for k, v := range extra {
		cfg[k] = v
	}
	callbacks, _ := json.Marshal([]msgLink{
		{Label: "Retry", URL: fx.target.srv.URL + "/api/episodes/5/retry"},
		{Label: "Try another release", URL: fx.target.srv.URL + "/api/episodes/5/another"},
	})
	ev := testEvent()
	ev.Annotations = map[string]string{"callbacks": string(callbacks)}
	fx.store = &fakeCallbackStore{
		provs: []models.ProviderConfig{{Name: "tg", Kind: "telegram", Enabled: true, Config: cfg, Secret: fixtureToken}},
		events: map[int64]models.StoredEvent{
			42: {Event: ev, ID: 42, ReceivedAt: fx.now.Add(-time.Hour)},
		},
	}
	fx.svc = NewTelegramCallbacks(fx.store, true, slog.New(slog.DiscardHandler))
	fx.svc.now = func() time.Time { return fx.now }
	fx.svc.rescan = 20 * time.Millisecond
	fx.svc.pollWait = 1
	fx.svc.retryBase = 10 * time.Millisecond
	fx.bot = bot{key: fx.tg.srv.URL + "\x00" + fixtureToken, apiBase: fx.tg.srv.URL, token: fixtureToken}
	return fx
}

// press builds the callback query of pressing entry index of event 42's
// callbacks in chat, from user, on a message that also has a link button.
func (fx *callbackFixture) press(chat, user int64, index int) tgCallbackQuery {
	data := signCallback(callbackKey(fixtureToken), "777", 42, index)
	return tgCallbackQuery{
		ID:   "cq-1",
		From: tgUser{ID: user, FirstName: "Owner"},
		Data: data,
		Message: &tgMessage{
			MessageID: 99,
			Chat:      tgChat{ID: chat},
			ReplyMarkup: &tgKeyboard{InlineKeyboard: [][]tgButton{
				{{Text: "Retry", CallbackData: signCallback(callbackKey(fixtureToken), "777", 42, 0)},
					{Text: "Try another release", CallbackData: signCallback(callbackKey(fixtureToken), "777", 42, 1)}},
				{{Text: "Open in Omni-Media", URL: "https://media.example/#/episode/5"}},
			}},
		},
	}
}

func (fx *callbackFixture) answerOf(t *testing.T) (text string, alert bool) {
	t.Helper()
	answers := fx.tg.callsTo("answerCallbackQuery")
	if len(answers) != 1 {
		t.Fatalf("answerCallbackQuery called %d times (calls %v)", len(answers), fx.tg.methods())
	}
	text, _ = answers[0].JSON["text"].(string)
	alert, _ = answers[0].JSON["show_alert"].(bool)
	return text, alert
}

func TestCallbackPressRunsAndMarksDone(t *testing.T) {
	fx := newCallbackFixture(t, nil)
	fx.svc.handle(context.Background(), fx.bot, fx.press(fixtureChat, 1, 0))

	reqs := fx.target.got()
	if len(reqs) != 1 {
		t.Fatalf("target got %d requests", len(reqs))
	}
	if r := reqs[0]; r.Method != http.MethodPost || r.URL.Path != "/api/episodes/5/retry" || r.Header.Get("X-Media-Request") != "1" {
		t.Fatalf("callback request %s %s X-Media-Request=%q", r.Method, r.URL.Path, r.Header.Get("X-Media-Request"))
	}
	if text, alert := fx.answerOf(t); text != "Retry…" || alert {
		t.Fatalf("answer %q alert=%v", text, alert)
	}
	if got := fx.tg.methods(); strings.Join(got, ",") != "answerCallbackQuery,editMessageReplyMarkup" {
		t.Fatalf("calls %v: the press must be answered before the request, then marked", got)
	}
	edit := fx.tg.callsTo("editMessageReplyMarkup")[0]
	if edit.JSON["chat_id"].(float64) != 777 || edit.JSON["message_id"].(float64) != 99 {
		t.Fatalf("edited %v/%v", edit.JSON["chat_id"], edit.JSON["message_id"])
	}
	kb := keyboardOf(t, edit)
	if b := kb.InlineKeyboard[0][0]; b.Text != "✓ Retry" || b.CallbackData != callbackDone {
		t.Fatalf("pressed button became %+v", b)
	}
	if b := kb.InlineKeyboard[0][1]; b.Text != "Try another release" || b.CallbackData == callbackDone {
		t.Fatalf("other button changed: %+v", b)
	}
	if b := kb.InlineKeyboard[1][0]; b.URL != "https://media.example/#/episode/5" {
		t.Fatalf("link button lost: %+v", b)
	}

	// Pressing the done button only answers.
	fx.svc.handle(context.Background(), fx.bot, tgCallbackQuery{ID: "cq-2", Data: callbackDone, Message: &tgMessage{Chat: tgChat{ID: fixtureChat}}})
	if len(fx.target.got()) != 1 {
		t.Fatal("the done button called the target")
	}
}

func TestCallbackFailureIsRepliedTo(t *testing.T) {
	fx := newCallbackFixture(t, nil)
	fx.target.status, fx.target.body = http.StatusConflict, `{"error":"the episode is already being searched"}`
	fx.svc.handle(context.Background(), fx.bot, fx.press(fixtureChat, 1, 1))

	if reqs := fx.target.got(); len(reqs) != 1 || reqs[0].URL.Path != "/api/episodes/5/another" {
		t.Fatalf("target requests %v", reqs)
	}
	if n := len(fx.tg.callsTo("editMessageReplyMarkup")); n != 0 {
		t.Fatal("a failed callback marked its button done")
	}
	replies := fx.tg.callsTo("sendMessage")
	if len(replies) != 1 {
		t.Fatalf("calls %v, want a reply", fx.tg.methods())
	}
	text, _ := replies[0].JSON["text"].(string)
	if !strings.Contains(text, "Try another release didn't work: the episode is already being searched") {
		t.Fatalf("reply %q", text)
	}
	rp, _ := replies[0].JSON["reply_parameters"].(map[string]any)
	if rp["message_id"].(float64) != 99 || replies[0].JSON["chat_id"].(float64) != 777 {
		t.Fatalf("reply not threaded: %v", replies[0].JSON)
	}

	// A redirect is a failure too (a POST is never followed elsewhere).
	fx.target.status, fx.target.body = http.StatusFound, ""
	fx.svc.handle(context.Background(), fx.bot, fx.press(fixtureChat, 1, 0))
	if n := len(fx.tg.callsTo("sendMessage")); n != 2 {
		t.Fatalf("redirect not reported: %d replies", n)
	}
}

func TestCallbackRefusals(t *testing.T) {
	cases := []struct {
		name  string
		extra map[string]any
		setup func(fx *callbackFixture) tgCallbackQuery
		want  string
	}{
		{
			name:  "another chat",
			setup: func(fx *callbackFixture) tgCallbackQuery { return fx.press(888, 1, 0) },
			want:  "don't work in this chat",
		},
		{
			name:  "user not allowed",
			extra: map[string]any{"callback_user_ids": []any{float64(1)}},
			setup: func(fx *callbackFixture) tgCallbackQuery { return fx.press(fixtureChat, 2, 0) },
			want:  "can't use these buttons",
		},
		{
			name: "forged index",
			setup: func(fx *callbackFixture) tgCallbackQuery {
				cq := fx.press(fixtureChat, 1, 0)
				cq.Data = strings.Replace(cq.Data, "c1:42:0:", "c1:42:1:", 1)
				return cq
			},
			want: "no longer works",
		},
		{
			name: "signed by another bot",
			setup: func(fx *callbackFixture) tgCallbackQuery {
				cq := fx.press(fixtureChat, 1, 0)
				cq.Data = signCallback(callbackKey("other-token"), "777", 42, 0)
				return cq
			},
			want: "no longer works",
		},
		{
			name: "event gone",
			setup: func(fx *callbackFixture) tgCallbackQuery {
				cq := fx.press(fixtureChat, 1, 0)
				cq.Data = signCallback(callbackKey(fixtureToken), "777", 43, 0)
				return cq
			},
			want: "no longer works",
		},
		{
			name: "index past the annotation",
			setup: func(fx *callbackFixture) tgCallbackQuery {
				cq := fx.press(fixtureChat, 1, 0)
				cq.Data = signCallback(callbackKey(fixtureToken), "777", 42, 5)
				return cq
			},
			want: "no longer works",
		},
		{
			name:  "expired",
			extra: map[string]any{"callback_ttl": "30m"},
			setup: func(fx *callbackFixture) tgCallbackQuery { return fx.press(fixtureChat, 1, 0) },
			want:  "expired",
		},
		{
			name:  "host no longer allowed",
			extra: map[string]any{"callback_hosts": []any{"media.example"}},
			setup: func(fx *callbackFixture) tgCallbackQuery { return fx.press(fixtureChat, 1, 0) },
			want:  "address is not allowed",
		},
		{
			name: "provider disabled",
			setup: func(fx *callbackFixture) tgCallbackQuery {
				fx.store.provs[0].Enabled = false
				return fx.press(fixtureChat, 1, 0)
			},
			want: "don't work in this chat",
		},
		{
			name: "no message",
			setup: func(fx *callbackFixture) tgCallbackQuery {
				cq := fx.press(fixtureChat, 1, 0)
				cq.Message = nil
				return cq
			},
			want: "no longer works",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCallbackFixture(t, tc.extra)
			fx.svc.handle(context.Background(), fx.bot, tc.setup(fx))
			if n := len(fx.target.got()); n != 0 {
				t.Fatalf("target called %d times", n)
			}
			text, alert := fx.answerOf(t)
			if !alert || !strings.Contains(text, tc.want) {
				t.Fatalf("answer %q alert=%v, want an alert with %q", text, alert, tc.want)
			}
		})
	}
}

func TestCallbackPressTooOldToAnswerIsNotRun(t *testing.T) {
	fx := newCallbackFixture(t, nil)
	fx.tg.answer("answerCallbackQuery", func(tgCall) (int, string) {
		return http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: query is too old and response timeout expired or query ID is invalid"}`
	})
	fx.svc.handle(context.Background(), fx.bot, fx.press(fixtureChat, 1, 0))
	if n := len(fx.target.got()); n != 0 {
		t.Fatalf("an unanswerable press ran the callback (%d requests)", n)
	}
}

func TestCallbackPollingEndToEnd(t *testing.T) {
	fx := newCallbackFixture(t, nil)
	// A second Telegram provider on another bot without callback_hosts: never polled.
	fx.store.provs = append(fx.store.provs, models.ProviderConfig{Name: "tg-plain", Kind: "telegram", Enabled: true,
		Config: map[string]any{"chat_id": "1", "api_base": fx.tg.srv.URL}, Secret: "plain-token"})

	press := fx.press(fixtureChat, 1, 0)
	var mu sync.Mutex
	served := false
	fx.tg.answer("getUpdates", func(c tgCall) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		if !served {
			served = true
			b, _ := json.Marshal([]tgUpdate{{UpdateID: 500}, {UpdateID: 501, CallbackQuery: &press}})
			return http.StatusOK, fmt.Sprintf(`{"ok":true,"result":%s}`, b)
		}
		time.Sleep(20 * time.Millisecond) // a short long poll
		return http.StatusOK, `{"ok":true,"result":[]}`
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		fx.svc.Run(ctx)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(fx.tg.callsTo("editMessageReplyMarkup")) == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("press never handled; calls %v", fx.tg.methods())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	if n := len(fx.target.got()); n != 1 {
		t.Fatalf("target called %d times", n)
	}
	polls := fx.tg.callsTo("getUpdates")
	if len(polls) < 2 {
		t.Fatalf("getUpdates called %d times", len(polls))
	}
	for _, p := range polls {
		if p.Token != fixtureToken {
			t.Fatalf("polled a bot without callback_hosts (token %q)", p.Token)
		}
		allowed, _ := p.JSON["allowed_updates"].([]any)
		if len(allowed) != 1 || allowed[0] != "callback_query" {
			t.Fatalf("allowed_updates %v", p.JSON["allowed_updates"])
		}
	}
	if off := polls[1].JSON["offset"].(float64); off != 502 {
		t.Fatalf("second poll offset %v, want 502", off)
	}
	last := polls[len(polls)-1]
	if last.JSON["offset"].(float64) != 502 || last.JSON["timeout"].(float64) != 0 {
		t.Fatalf("last getUpdates %v, want the confirming call (offset 502, timeout 0)", last.JSON)
	}
}

func TestCallbackPollingBacksOffOnConflict(t *testing.T) {
	fx := newCallbackFixture(t, nil)
	fx.tg.answer("getUpdates", func(tgCall) (int, string) {
		return http.StatusConflict, `{"ok":false,"error_code":409,"description":"Conflict: can't use getUpdates method while webhook is active"}`
	})
	fx.svc.retryBase, fx.svc.retryMax = 40*time.Millisecond, 40*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	fx.svc.Run(ctx)
	// About one call per 40 ms; without the wait it would be thousands.
	if n := len(fx.tg.callsTo("getUpdates")); n < 2 || n > 12 {
		t.Fatalf("getUpdates called %d times in 300 ms", n)
	}
}
