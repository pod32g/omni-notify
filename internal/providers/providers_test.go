package providers

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/pod32g/omni-notify/internal/models"
)

func parseIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("bad test IP %q", s)
	}
	return ip
}

func testEvent() models.Event {
	return models.Event{
		EventID:   "pihole-down",
		Type:      "alert",
		Source:    "homelab",
		Status:    models.StatusFiring,
		Severity:  models.SeverityCritical,
		Title:     "Pi-hole Down",
		Summary:   "not responding",
		Labels:    map[string]string{"service": "pihole"},
		Timestamp: time.Date(2026, 6, 28, 20, 0, 0, 0, time.UTC),
	}
}

// captureServer records the last request body and returns the given status.
func captureServer(t *testing.T, status int) (*httptest.Server, *[]byte) {
	t.Helper()
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &body
}

func TestDiscordSend(t *testing.T) {
	srv, body := captureServer(t, http.StatusNoContent)
	p, err := newDiscord(srv.Client(), map[string]any{"username": "bot"}, srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err != nil {
		t.Fatal(err)
	}
	var payload discordPayload
	if err := json.Unmarshal(*body, &payload); err != nil {
		t.Fatalf("invalid discord payload: %v\n%s", err, *body)
	}
	if payload.Username != "bot" || len(payload.Embeds) != 1 {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	if !strings.Contains(payload.Embeds[0].Title, "Pi-hole Down") {
		t.Fatalf("title missing: %q", payload.Embeds[0].Title)
	}
}

func TestDiscordRequiresSecret(t *testing.T) {
	if _, err := newDiscord(http.DefaultClient, nil, "", true); err == nil {
		t.Fatal("expected error for missing webhook URL")
	}
}

func TestDiscordNon2xxIsError(t *testing.T) {
	srv, _ := captureServer(t, http.StatusInternalServerError)
	p, _ := newDiscord(srv.Client(), nil, srv.URL, true)
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err == nil {
		t.Fatal("expected error on 500 response")
	}
}

func TestSlackSend(t *testing.T) {
	srv, body := captureServer(t, http.StatusOK)
	p, _ := newSlack(srv.Client(), nil, srv.URL, true)
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err != nil {
		t.Fatal(err)
	}
	var payload slackPayload
	if err := json.Unmarshal(*body, &payload); err != nil {
		t.Fatalf("invalid slack payload: %v", err)
	}
	if len(payload.Attachments) != 1 || payload.Attachments[0].Color != "danger" {
		t.Fatalf("unexpected slack payload: %+v", payload)
	}
}

func TestWebhookDefaultJSON(t *testing.T) {
	srv, body := captureServer(t, http.StatusOK)
	p, _ := newWebhook(srv.Client(), nil, srv.URL, true)
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err != nil {
		t.Fatal(err)
	}
	var ev models.Event
	if err := json.Unmarshal(*body, &ev); err != nil {
		t.Fatalf("expected event JSON body: %v\n%s", err, *body)
	}
	if ev.EventID != "pihole-down" {
		t.Fatalf("event not serialised: %+v", ev)
	}
}

func TestWebhookTemplate(t *testing.T) {
	srv, body := captureServer(t, http.StatusOK)
	cfg := map[string]any{"template": "ALERT: {{.Title}} ({{.Severity}})", "content_type": "text/plain"}
	p, err := newWebhook(srv.Client(), cfg, srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err != nil {
		t.Fatal(err)
	}
	if got := string(*body); got != "ALERT: Pi-hole Down (critical)" {
		t.Fatalf("template render mismatch: %q", got)
	}
}

func TestWebhookInvalidTemplate(t *testing.T) {
	if _, err := newWebhook(http.DefaultClient, map[string]any{"template": "{{.Bad"}, "https://x", true); err == nil {
		t.Fatal("expected error for invalid template")
	}
}

func TestSMTPValidation(t *testing.T) {
	cases := []map[string]any{
		{},                         // missing host
		{"host": "h"},              // missing from
		{"host": "h", "from": "f"}, // missing to
		{"host": "h", "from": "f", "to": []any{"x"}, "tls": "bogus"}, // bad tls mode
	}
	for i, cfg := range cases {
		if _, err := newSMTP(cfg, "pw"); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
	if _, err := newSMTP(map[string]any{"host": "h", "from": "f", "to": []any{"a@b.c"}}, "pw"); err != nil {
		t.Fatalf("valid smtp config rejected: %v", err)
	}
}

func TestSMTPBuildMessageSanitizesHeaders(t *testing.T) {
	p, err := newSMTP(map[string]any{"host": "h", "from": "from@x", "to": []any{"to@y"}}, "pw")
	if err != nil {
		t.Fatal(err)
	}
	sp := p.(*smtpProvider)
	ev := testEvent()
	ev.Title = "Injected\r\nBcc: evil@x" // header injection attempt
	msg := string(sp.buildMessage(models.NotificationMessage{Event: ev}))

	headerBlock, _, _ := strings.Cut(msg, "\r\n\r\n")
	for _, line := range strings.Split(headerBlock, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") {
			t.Fatalf("header injection created a Bcc header:\n%s", msg)
		}
	}
	if !strings.Contains(msg, "Subject: ") || !strings.Contains(msg, "service: pihole") {
		t.Fatalf("message missing subject or labels:\n%s", msg)
	}
}

func TestProviderURLSchemeValidation(t *testing.T) {
	bad := []string{"file:///etc/passwd", "gopher://example.com/x", "ftp://host/x", "notaurl", "https://"}
	for _, secret := range bad {
		if _, err := newDiscord(http.DefaultClient, nil, secret, true); err == nil {
			t.Errorf("discord accepted bad URL %q", secret)
		}
		if _, err := newSlack(http.DefaultClient, nil, secret, true); err == nil {
			t.Errorf("slack accepted bad URL %q", secret)
		}
		if _, err := newWebhook(http.DefaultClient, nil, secret, true); err == nil {
			t.Errorf("webhook accepted bad URL %q", secret)
		}
	}
	for _, secret := range []string{"http://127.0.0.1:9/x", "https://example.com/hook"} {
		if _, err := newDiscord(http.DefaultClient, nil, secret, true); err != nil {
			t.Errorf("discord rejected valid URL %q: %v", secret, err)
		}
	}
}

func TestPrivateTargetBlocking(t *testing.T) {
	blocked := []string{
		"http://localhost/x",
		"http://127.0.0.1/x",
		"http://10.1.2.3/x",
		"http://192.168.1.1/x",
		"http://172.16.0.1/x",
		"http://169.254.1.1/x", // link-local
		"http://[::1]/x",       // loopback v6
	}
	for _, secret := range blocked {
		if _, err := newWebhook(http.DefaultClient, nil, secret, false); err == nil {
			t.Errorf("expected %q to be blocked when allowPrivate=false", secret)
		}
		// allowPrivate=true permits them.
		if _, err := newWebhook(http.DefaultClient, nil, secret, true); err != nil {
			t.Errorf("expected %q to be allowed when allowPrivate=true: %v", secret, err)
		}
	}
	// Public hostnames are allowed at build time (DNS-resolved IPs are checked by
	// the guarded dialer at connection time).
	if _, err := newWebhook(http.DefaultClient, nil, "https://example.com/hook", false); err != nil {
		t.Errorf("public host should be allowed at build time: %v", err)
	}
}

func TestIsPrivateIP(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":         true,
		"10.0.0.1":          true,
		"192.168.1.1":       true,
		"172.16.5.4":        true,
		"169.254.0.1":       true,
		"224.0.0.1":         true, // multicast
		"::1":               true,
		"100.64.0.1":        true, // RFC 6598 CGNAT
		"100.127.255.254":   true, // CGNAT upper
		"::ffff:10.0.0.1":   true, // IPv4-mapped private
		"::ffff:100.64.0.1": true, // IPv4-mapped CGNAT
		"8.8.8.8":           false,
		"1.1.1.1":           false,
		"100.128.0.1":       false, // just outside CGNAT
	}
	for ipStr, want := range cases {
		if got := isPrivateIP(parseIP(t, ipStr)); got != want {
			t.Errorf("isPrivateIP(%s) = %v, want %v", ipStr, got, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("short string changed: %q", got)
	}
	if got := truncate("abcdefghij", 5); utf8.RuneCountInString(got) != 5 || got != "abcd…" {
		t.Errorf("ascii truncate = %q (runes=%d), want abcd…", got, utf8.RuneCountInString(got))
	}
	multibyte := truncate("日本語テキストデータ", 5)
	if !utf8.ValidString(multibyte) {
		t.Errorf("multibyte truncate produced invalid UTF-8: %q", multibyte)
	}
	if utf8.RuneCountInString(multibyte) != 5 {
		t.Errorf("multibyte truncate runes = %d, want 5: %q", utf8.RuneCountInString(multibyte), multibyte)
	}
}

func TestWebhookAuthHeaderSecret(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	secret := `{"url":"` + srv.URL + `","auth_header":"Authorization","auth_value":"Bearer s3cr3t"}`
	p, err := newWebhook(srv.Client(), nil, secret, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer s3cr3t" {
		t.Fatalf("auth header not sent, got %q", gotAuth)
	}
}

func TestWebhookSecretJSONValidation(t *testing.T) {
	if _, err := newWebhook(http.DefaultClient, nil, `{"auth_header":"X"}`, true); err == nil {
		t.Error("expected error for JSON secret without url")
	}
	if _, err := newWebhook(http.DefaultClient, nil, `{"url":"https://x/y","auth_value":"v"}`, true); err == nil {
		t.Error("expected error for auth_value without auth_header")
	}
}

func TestRegistryDefault(t *testing.T) {
	r := NewDefault(nil, true)
	for _, kind := range []string{"discord", "slack", "webhook", "smtp"} {
		if !r.Has(kind) {
			t.Errorf("missing default provider kind %q", kind)
		}
	}
	if _, err := r.Build("nope", nil, "x"); err == nil {
		t.Fatal("expected error for unknown kind")
	}
}
