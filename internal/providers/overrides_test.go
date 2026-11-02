package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/pod32g/omni-notify/internal/models"
)

// recorder is a fake channel API: it records every request and answers with
// answer(r) (default 200 "{}").
type recorder struct {
	srv *httptest.Server

	mu     sync.Mutex
	reqs   []recorded
	answer func(r recorded) (int, string)
}

type recorded struct {
	Method, Path string
	Query        url.Values
	Header       http.Header
	Body         []byte
}

// json decodes the request's body.
func (r recorded) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("%s %s body %q: %v", r.Method, r.Path, r.Body, err)
	}
	return m
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	rec := &recorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got := recorded{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone(), Body: body}
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, got)
		answer := rec.answer
		rec.mu.Unlock()
		status, out := http.StatusOK, "{}"
		if answer != nil {
			status, out = answer(got)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, out)
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (rec *recorder) all() []recorded {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]recorded(nil), rec.reqs...)
}

func (rec *recorder) last(t *testing.T) recorded {
	t.Helper()
	all := rec.all()
	if len(all) == 0 {
		t.Fatal("no request")
	}
	return all[len(all)-1]
}

// overridden returns testEvent with message text and an override of kind.
func overridden(text, kind, override string) models.Event {
	ev := testEvent()
	ev.Fingerprint = "fp-1"
	ev.Message = &models.Message{Text: text, Channels: map[string]json.RawMessage{kind: json.RawMessage(override)}}
	return ev
}

func build(t *testing.T, kind string, cfg map[string]any, secret string, store MessageStore) Provider {
	t.Helper()
	p, err := NewDefault(http.DefaultClient, true, WithMessageStore(store)).Build(kind, cfg, secret)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func send(t *testing.T, p Provider, ref int64, ev models.Event) {
	t.Helper()
	if err := p.Send(context.Background(), models.NotificationMessage{Event: ev, EventRef: ref}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateOverrides(t *testing.T) {
	good := map[string]string{
		"discord":    `{"content":"hi","embeds":[{"title":"x","color":15158332}],"components":[{"type":1,"components":[{"type":2,"style":5,"label":"Open","url":"https://a"}]}],"flags":["suppress_notifications"],"thread_id":"123","ref":"r","edit":"r","delete":"o"}`,
		"slack":      `{"text":"x","blocks":[{"type":"section","text":{"type":"mrkdwn","text":"*x*"}}],"unfurl_links":false,"thread_ts":"1.2"}`,
		"teams":      `{"card":{"type":"AdaptiveCard","version":"1.4","body":[]}}`,
		"googlechat": `{"text":"x","cardsV2":[{"cardId":"c","card":{}}],"threadKey":"pihole"}`,
		"ntfy":       `{"priority":5,"tags":["warning"],"markdown":true,"click":"https://a","actions":[{"action":"view","label":"Open","url":"https://a"},{"action":"copy","label":"ID","value":"7"}],"ref":"rec:1"}`,
		"gotify":     `{"markdown":true,"click_url":"https://a","priority":8,"extras":{"x::y":{"z":1}}}`,
		"pushover":   `{"html":true,"url":"https://a","url_title":"Open","priority":2,"retry":60,"expire":3600,"sound":"siren"}`,
		"matrix":     `{"formatted_body":"<b>x</b>","msgtype":"m.notice","mentions":{"room":true},"ref":"r","thread":"t","pin":true,"react":{"emoji":"👍"}}`,
		"smtp":       `{"html":"<p>x</p>"}`,
		"pagerduty":  `{"summary":"x","severity":"critical","custom_details":{"a":1},"links":[{"href":"https://a","text":"A"}],"images":[{"src":"https://a/i.png"}],"event_action":"acknowledge"}`,
		"opsgenie":   `{"priority":"P1","tags":["dns"],"details":{"host":"pi"},"actions":["Restart"],"responders":[{"type":"team","name":"ops"}]}`,
		"twilio":     `{"body":"x","media_url":["https://a/i.png"]}`,
	}
	for kind, raw := range good {
		if err := ValidateMessage(&models.Message{Channels: map[string]json.RawMessage{kind: json.RawMessage(raw)}}); err != nil {
			t.Errorf("%s: valid override refused: %v", kind, err)
		}
	}
	bad := map[string][]string{
		"discord":    {`{"contnt":"x"}`, `{"embeds":[1]}`, `{"flags":["loud"]}`, `{"flags":["is_components_v2"],"content":"x"}`, `{"thread_id":"abc"}`, `{"send":false}`},
		"slack":      {`{"blocks":"x"}`, `{"icon_url":"ftp://a"}`},
		"teams":      {`{"card":{"type":"MessageCard"}}`, `{"card":{"type":"AdaptiveCard"},"message_card":{}}`},
		"googlechat": {`{"cards":[]}`},
		"ntfy":       {`{"priority":9}`, `{"actions":[{"action":"open","label":"x"}]}`, `{"actions":[{},{},{},{}]}`, `{"ref":"a","edit":"b"}`, `{"send":false}`},
		"gotify":     {`{"extras":[]}`},
		"pushover":   {`{"html":true,"monospace":true}`, `{"priority":2}`, `{"priority":3}`, `{"url_title":"` + strings.Repeat("x", 101) + `"}`},
		"matrix":     {`{"msgtype":"m.image"}`, `{"react":{"to":"x"}}`, `{"send":false,"pin":true}`},
		"smtp":       {`{"text":"x"}`},
		"pagerduty":  {`{"severity":"sev1"}`, `{"event_action":"close"}`, `{"links":[{"text":"x"}]}`},
		"opsgenie":   {`{"priority":"P9"}`, `{"message":"` + strings.Repeat("x", 131) + `"}`},
		"twilio":     {`{"media_url":["file:///etc/passwd"]}`},
	}
	for kind, cases := range bad {
		for _, raw := range cases {
			err := ValidateMessage(&models.Message{Channels: map[string]json.RawMessage{kind: json.RawMessage(raw)}})
			if err == nil || !strings.HasPrefix(err.Error(), "message."+kind+": ") {
				t.Errorf("%s %s: got %v", kind, raw, err)
			}
		}
	}
}

func TestDiscordOverrideRefsAndFallback(t *testing.T) {
	rec := newRecorder(t)
	next := 110
	rec.answer = func(r recorded) (int, string) {
		if r.Method == http.MethodPost {
			if strings.Contains(string(r.Body), `"bad"`) {
				return http.StatusBadRequest, `{"message":"Invalid Form Body"}`
			}
			next++
			return http.StatusOK, `{"id":"` + itoa(next) + `","channel_id":"9"}`
		}
		if r.Method == http.MethodPatch && strings.HasSuffix(r.Path, "/999") {
			return http.StatusNotFound, `{"message":"Unknown Message"}`
		}
		return http.StatusOK, `{}`
	}
	store := newMemMessages()
	hook := rec.srv.URL + "/api/webhooks/42/secret-token"
	p := build(t, "discord", map[string]any{"username": "Omni"}, hook, store)

	send(t, p, 1, overridden("started", "discord", `{"content":"🎬 started","embeds":[{"title":"CatinaJacket","color":3447003}],
		"components":[{"type":1,"components":[{"type":2,"style":5,"label":"Watch","url":"https://fansly.com/x"}]}],
		"flags":["suppress_notifications"],"thread_id":"555","ref":"rec"}`))
	r := rec.last(t)
	if r.Path != "/api/webhooks/42/secret-token" || r.Query.Get("wait") != "true" || r.Query.Get("with_components") != "true" || r.Query.Get("thread_id") != "555" {
		t.Fatalf("POST %s?%s", r.Path, r.Query.Encode())
	}
	body := r.json(t)
	if body["content"] != "🎬 started" || body["flags"] != float64(1<<12) || body["username"] != "Omni" || len(body["embeds"].([]any)) != 1 {
		t.Fatalf("body %v", body)
	}

	// Edit by ref: PATCH the remembered message, clearing what this edit doesn't set.
	send(t, p, 2, overridden("failed", "discord", `{"content":"❌ failed","thread_id":"555","edit":"rec"}`))
	r = rec.last(t)
	if r.Method != http.MethodPatch || r.Path != "/api/webhooks/42/secret-token/messages/111" || r.Query.Get("thread_id") != "555" {
		t.Fatalf("%s %s?%s", r.Method, r.Path, r.Query.Encode())
	}
	if b := r.json(t); b["content"] != "❌ failed" || b["embeds"] != nil || b["username"] != nil {
		t.Fatalf("edit body %v", b)
	}
	if !hasKey(r.Body, "embeds") {
		t.Fatal("edit didn't clear the embeds")
	}

	// Refused content: the plain embed goes out instead.
	send(t, p, 3, overridden("plain text", "discord", `{"content":"bad"}`))
	r = rec.last(t)
	if b := r.json(t); b["content"] != nil || len(b["embeds"].([]any)) != 1 {
		t.Fatalf("fallback body %v", b)
	}

	// A message gone (404) is sent anew.
	_ = store.PutSentMessage(context.Background(), models.SentMessage{Channel: "discord", Account: accountOf(hook), Ref: "gone", MessageID: "999"})
	send(t, p, 4, overridden("again", "discord", `{"content":"again","edit":"gone"}`))
	if r := rec.last(t); r.Method != http.MethodPost {
		t.Fatalf("last %s %s", r.Method, r.Path)
	}

	// Delete only.
	send(t, p, 5, overridden("", "discord", `{"send":false,"delete":"gone"}`))
	if r := rec.last(t); r.Method != http.MethodDelete || !strings.HasSuffix(r.Path, "/messages/113") {
		t.Fatalf("last %s %s", r.Method, r.Path)
	}
}

// hasKey reports whether the JSON body has key (even as null).
func hasKey(body []byte, key string) bool {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(body, &m)
	_, ok := m[key]
	return ok
}

// itoa formats n with three digits.
func itoa(n int) string { return fmt.Sprintf("%03d", n) }

func TestSlackTeamsGoogleChatOverrides(t *testing.T) {
	rec := newRecorder(t)
	rec.answer = func(r recorded) (int, string) {
		if strings.Contains(string(r.Body), "refuse-me") {
			return http.StatusBadRequest, "invalid_blocks"
		}
		return http.StatusOK, "ok"
	}

	slack := build(t, "slack", nil, rec.srv.URL+"/slack", nil)
	send(t, slack, 1, overridden("fallback text", "slack", `{"blocks":[{"type":"section","text":{"type":"mrkdwn","text":"*bold*"}}],"unfurl_links":false,"thread_ts":"1.5"}`))
	if b := rec.last(t).json(t); b["text"] != "fallback text" || len(b["blocks"].([]any)) != 1 || b["unfurl_links"] != false || b["thread_ts"] != "1.5" || b["attachments"] != nil {
		t.Fatalf("slack %v", b)
	}
	send(t, slack, 2, overridden("x", "slack", `{"blocks":[{"type":"refuse-me"}]}`))
	if b := rec.last(t).json(t); b["blocks"] != nil || len(b["attachments"].([]any)) != 1 {
		t.Fatalf("slack fallback %v", b)
	}

	teams := build(t, "teams", nil, rec.srv.URL+"/teams", nil)
	send(t, teams, 1, overridden("x", "teams", `{"card":{"type":"AdaptiveCard","version":"1.4","body":[{"type":"TextBlock","text":"hi"}]}}`))
	b := rec.last(t).json(t)
	att := b["attachments"].([]any)[0].(map[string]any)
	if b["type"] != "message" || att["contentType"] != "application/vnd.microsoft.card.adaptive" || att["content"].(map[string]any)["version"] != "1.4" {
		t.Fatalf("teams %v", b)
	}
	if _, ok := att["contentUrl"]; !ok {
		t.Fatal("teams: contentUrl must be sent (null)")
	}

	chat := build(t, "googlechat", nil, rec.srv.URL+"/chat?key=k", nil)
	send(t, chat, 1, overridden("x", "googlechat", `{"cardsV2":[{"cardId":"a","card":{"header":{"title":"Pi-hole"}}}],"threadKey":"pihole"}`))
	r := rec.last(t)
	if r.Query.Get("messageReplyOption") != "REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD" || r.Query.Get("key") != "k" {
		t.Fatalf("chat query %v", r.Query)
	}
	if b := r.json(t); b["thread"].(map[string]any)["threadKey"] != "pihole" || len(b["cardsV2"].([]any)) != 1 || b["text"] != "x" {
		t.Fatalf("chat %v", b)
	}
}

func TestNtfyOverride(t *testing.T) {
	rec := newRecorder(t)
	rec.answer = func(r recorded) (int, string) {
		if strings.Contains(string(r.Body), "refuse-me") {
			return http.StatusBadRequest, `{"code":40001,"error":"invalid request"}`
		}
		return http.StatusOK, `{}`
	}
	p := build(t, "ntfy", map[string]any{"token": "tk", "tags": "a,b"}, rec.srv.URL+"/ntfy/alerts", nil)

	send(t, p, 1, overridden("Pi-hole is **down**", "ntfy", `{"markdown":true,"priority":5,"click":"https://grafana/d",
		"actions":[{"action":"http","label":"Restart","url":"https://ha/restart"}],"ref":"rec:6642"}`))
	r := rec.last(t)
	if r.Method != http.MethodPost || r.Path != "/ntfy/" || r.Header.Get("Authorization") != "Bearer tk" {
		t.Fatalf("%s %s auth %q", r.Method, r.Path, r.Header.Get("Authorization"))
	}
	b := r.json(t)
	if b["topic"] != "alerts" || b["message"] != "Pi-hole is **down**" || b["markdown"] != true || b["priority"] != float64(5) || len(b["tags"].([]any)) != 2 {
		t.Fatalf("ntfy %v", b)
	}
	sid := b["sequence_id"].(string)
	if !ntfySequenceID.MatchString(sid) || sid != ntfySID("rec:6642") || sid == "rec:6642" {
		t.Fatalf("sequence_id %q: want a valid hash of the ref", sid)
	}

	// Resolved: clear and delete the notification, send nothing.
	send(t, p, 2, overridden("", "ntfy", `{"send":false,"clear":"rec:6642","delete":"rec:6642"}`))
	all := rec.all()
	clear, del := all[len(all)-2], all[len(all)-1]
	if clear.Method != http.MethodPut || clear.Path != "/ntfy/alerts/"+sid+"/clear" || del.Method != http.MethodDelete || del.Path != "/ntfy/alerts/"+sid {
		t.Fatalf("clear %s %s, delete %s %s", clear.Method, clear.Path, del.Method, del.Path)
	}

	// Refused JSON: the plain publish to the topic.
	send(t, p, 3, overridden("x", "ntfy", `{"title":"refuse-me"}`))
	if r := rec.last(t); r.Path != "/ntfy/alerts" || string(r.Body) != "x" || r.Header.Get("Title") == "" {
		t.Fatalf("fallback %s %s %q", r.Method, r.Path, r.Body)
	}
}

func TestGotifyPushoverOverrides(t *testing.T) {
	rec := newRecorder(t)
	gotify := build(t, "gotify", map[string]any{"url": rec.srv.URL}, "app-token", nil)
	send(t, gotify, 1, overridden("**down**", "gotify", `{"markdown":true,"click_url":"https://grafana","priority":9,"extras":{"client::notification":{"bigImageUrl":"https://a/b.png"}}}`))
	b := rec.last(t).json(t)
	extras := b["extras"].(map[string]any)
	if b["priority"] != float64(9) || extras["client::display"].(map[string]any)["contentType"] != "text/markdown" {
		t.Fatalf("gotify %v", b)
	}
	if n := extras["client::notification"].(map[string]any); n["bigImageUrl"] != "https://a/b.png" || n["click"] != nil {
		t.Fatalf("gotify extras: the given extras must win: %v", n)
	}

	push := build(t, "pushover", map[string]any{"user": "u", "api_url": rec.srv.URL}, "tok", nil)
	send(t, push, 1, overridden("<b>down</b>", "pushover", `{"html":true,"url":"https://grafana","url_title":"Dashboard","priority":2,"retry":60,"expire":3600,"sound":"siren"}`))
	form, _ := url.ParseQuery(string(rec.last(t).Body))
	for k, want := range map[string]string{"message": "<b>down</b>", "html": "1", "url": "https://grafana", "url_title": "Dashboard",
		"priority": "2", "retry": "60", "expire": "3600", "sound": "siren", "token": "tok", "user": "u"} {
		if form.Get(k) != want {
			t.Errorf("pushover %s = %q, want %q", k, form.Get(k), want)
		}
	}
}

func TestMatrixOverrideRefs(t *testing.T) {
	rec := newRecorder(t)
	var mu sync.Mutex
	n, pinned := 0, `{"pinned":["$old"]}`
	rec.answer = func(r recorded) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.Path, "/state/m.room.pinned_events") && r.Method == http.MethodGet:
			return http.StatusOK, pinned
		case strings.HasSuffix(r.Path, "/state/m.room.pinned_events"):
			pinned = string(r.Body)
			return http.StatusOK, `{"event_id":"$state"}`
		case strings.Contains(string(r.Body), "refuse-me"):
			return http.StatusBadRequest, `{"errcode":"M_BAD_JSON"}`
		}
		n++
		return http.StatusOK, `{"event_id":"$e` + itoa(n) + `"}`
	}
	store := newMemMessages()
	p := build(t, "matrix", map[string]any{"homeserver": rec.srv.URL, "room_id": "!r:hs", "msgtype": "m.notice"}, "tok", store)

	send(t, p, 1, overridden("Pi-hole down", "matrix", `{"formatted_body":"<b>Pi-hole down</b>","mentions":{"room":true},"ref":"pihole","pin":true}`))
	b := rec.all()[0].json(t)
	if b["format"] != "org.matrix.custom.html" || b["formatted_body"] != "<b>Pi-hole down</b>" || b["body"] != "Pi-hole down" || b["msgtype"] != "m.notice" || b["m.mentions"] == nil {
		t.Fatalf("matrix %v", b)
	}
	if !strings.Contains(pinned, `"$old"`) || !strings.Contains(pinned, `"$e001"`) {
		t.Fatalf("pinned %s", pinned)
	}

	// A reply in a thread under it.
	send(t, p, 2, overridden("still down", "matrix", `{"thread":"pihole","reply_to":"pihole"}`))
	rel := rec.last(t).json(t)["m.relates_to"].(map[string]any)
	if rel["rel_type"] != "m.thread" || rel["event_id"] != "$e001" || rel["is_falling_back"] != false || rel["m.in_reply_to"].(map[string]any)["event_id"] != "$e001" {
		t.Fatalf("relates %v", rel)
	}

	// Resolved: edit it, unpin it, react to it.
	send(t, p, 3, overridden("Pi-hole back", "matrix", `{"edit":"pihole","unpin":"pihole","react":{"emoji":"✅"}}`))
	var edit, react map[string]any
	for _, r := range rec.all() {
		if strings.Contains(r.Path, "/send/m.room.message/") && strings.Contains(string(r.Body), "m.replace") {
			edit = r.json(t)
		}
		if strings.Contains(r.Path, "/send/m.reaction/") {
			react = r.json(t)
		}
	}
	if edit == nil || edit["body"] != "* Pi-hole back" || edit["m.new_content"].(map[string]any)["body"] != "Pi-hole back" ||
		edit["m.relates_to"].(map[string]any)["event_id"] != "$e001" {
		t.Fatalf("edit %v", edit)
	}
	if react == nil || react["m.relates_to"].(map[string]any)["key"] != "✅" || react["m.relates_to"].(map[string]any)["event_id"] != "$e001" {
		t.Fatalf("react %v", react)
	}
	if strings.Contains(pinned, `"$e001"`) {
		t.Fatalf("still pinned: %s", pinned)
	}
	if m, _ := store.GetSentMessage(context.Background(), "matrix", accountOf(rec.srv.URL, "tok"), "!r:hs", "pihole"); m.MessageID != "$e001" {
		t.Fatalf("the ref must keep naming the original, got %q", m.MessageID)
	}

	// Redact only.
	send(t, p, 4, overridden("", "matrix", `{"send":false,"delete":"pihole"}`))
	if r := rec.last(t); r.Method != http.MethodPut || !strings.Contains(r.Path, "/redact/$e001/") {
		t.Fatalf("redact %s %s", r.Method, r.Path)
	}

	// Refused: the plain message.
	send(t, p, 5, overridden("plain", "matrix", `{"formatted_body":"refuse-me"}`))
	if b := rec.last(t).json(t); b["body"] != "plain" || b["formatted_body"] != nil {
		t.Fatalf("fallback %v", b)
	}
}

func TestSMTPHTMLAlternative(t *testing.T) {
	p, err := newSMTP(map[string]any{"host": "mail", "from": "a@b", "to": "c@d"}, "")
	if err != nil {
		t.Fatal(err)
	}
	ev := overridden("Recording failed", "smtp", `{"html":"<p><b>Recording failed</b> `+strings.Repeat("x", 1200)+`</p>"}`)
	ev.Message.Title = "❌ Recording failed"
	raw := string(p.(*smtpProvider).buildMessage(models.NotificationMessage{Event: ev}))
	for _, want := range []string{"Subject: =?UTF-8?q?", "Content-Type: multipart/alternative; boundary=",
		"Content-Type: text/plain; charset=UTF-8", "Content-Type: text/html; charset=UTF-8", "Content-Transfer-Encoding: quoted-printable", "Recording failed"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("missing %q in:\n%s", want, raw)
		}
	}
	for _, line := range strings.Split(raw, "\r\n") {
		if len(line) > 998 {
			t.Fatalf("a line of %d bytes", len(line))
		}
	}
}

func TestIncidentOverrides(t *testing.T) {
	rec := newRecorder(t)
	pd := build(t, "pagerduty", map[string]any{"events_url": rec.srv.URL + "/pd"}, "rk", nil)
	send(t, pd, 1, overridden("x", "pagerduty", `{"summary":"DNS down","component":"pihole","custom_details":{"host":"pi"},
		"links":[{"href":"https://grafana","text":"Dashboard"}],"images":[{"src":"https://a/i.png"}]}`))
	b := rec.last(t).json(t)
	pl := b["payload"].(map[string]any)
	if b["event_action"] != "trigger" || pl["summary"] != "DNS down" || pl["component"] != "pihole" || pl["custom_details"].(map[string]any)["host"] != "pi" ||
		len(b["links"].([]any)) != 1 || len(b["images"].([]any)) != 1 || pl["severity"] != "critical" {
		t.Fatalf("pagerduty %v", b)
	}
	send(t, pd, 2, overridden("x", "pagerduty", `{"event_action":"acknowledge"}`))
	if b := rec.last(t).json(t); b["event_action"] != "acknowledge" || b["payload"] != nil {
		t.Fatalf("acknowledge %v", b)
	}

	og := build(t, "opsgenie", map[string]any{"api_url": rec.srv.URL + "/og"}, "key", nil)
	send(t, og, 1, overridden("x", "opsgenie", `{"priority":"P1","tags":["dns"],"details":{"host":"pi"},"note":"paged"}`))
	if b := rec.last(t).json(t); b["priority"] != "P1" || b["note"] != "paged" || b["details"].(map[string]any)["host"] != "pi" || b["alias"] != "fp-1" {
		t.Fatalf("opsgenie %v", b)
	}
	ev := overridden("x", "opsgenie", `{"note":"fixed by restart"}`)
	ev.Status = models.StatusResolved
	send(t, og, 2, ev)
	if r := rec.last(t); !strings.HasSuffix(r.Path, "/og/fp-1/close") || r.json(t)["note"] != "fixed by restart" {
		t.Fatalf("close %s %s", r.Path, r.Body)
	}

	tw := build(t, "twilio", map[string]any{"account_sid": "AC1", "from": "+1", "to": "+2", "api_base": rec.srv.URL}, "tok", nil)
	send(t, tw, 1, overridden("x", "twilio", `{"body":"Camera: motion","media_url":["https://cam/a.jpg","https://cam/b.jpg"]}`))
	form, _ := url.ParseQuery(string(rec.last(t).Body))
	if form.Get("Body") != "Camera: motion" || len(form["MediaUrl"]) != 2 {
		t.Fatalf("twilio %v", form)
	}
}
