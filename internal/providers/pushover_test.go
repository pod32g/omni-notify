package providers

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/pod32g/omni-notify/internal/models"
)

func TestNewPushover_Send(t *testing.T) {
	srv, body := captureServer(t, http.StatusOK)
	cfg := map[string]any{
		"user":     "ukey123",
		"api_url":  srv.URL,
		"priority": 1,
		"device":   "phone",
	}
	p, err := newPushover(srv.Client(), cfg, "apptoken", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err != nil {
		t.Fatal(err)
	}
	// Pushover expects form-encoded parameters, not JSON.
	form, err := url.ParseQuery(string(*body))
	if err != nil {
		t.Fatalf("body is not form-encoded: %v\n%s", err, *body)
	}
	if form.Get("token") != "apptoken" {
		t.Fatalf("token missing/incorrect: %q", form.Get("token"))
	}
	if form.Get("user") != "ukey123" {
		t.Fatalf("user missing/incorrect: %q", form.Get("user"))
	}
	if !strings.Contains(form.Get("message"), "not responding") {
		t.Fatalf("message missing body: %q", form.Get("message"))
	}
	if !strings.Contains(form.Get("title"), "Pi-hole Down") {
		t.Fatalf("title missing: %q", form.Get("title"))
	}
	if form.Get("priority") != "1" {
		t.Fatalf("priority not sent: %q", form.Get("priority"))
	}
	if form.Get("device") != "phone" {
		t.Fatalf("device not sent: %q", form.Get("device"))
	}
}

func TestNewPushover_RequiresUser(t *testing.T) {
	if _, err := newPushover(http.DefaultClient, map[string]any{}, "apptoken", true); err == nil {
		t.Fatal("expected error for missing user")
	}
}

func TestNewPushover_RequiresToken(t *testing.T) {
	if _, err := newPushover(http.DefaultClient, map[string]any{"user": "u"}, "", true); err == nil {
		t.Fatal("expected error for missing token secret")
	}
}

func TestNewPushover_Non2xxIsError(t *testing.T) {
	srv, _ := captureServer(t, http.StatusInternalServerError)
	p, err := newPushover(srv.Client(), map[string]any{"user": "u", "api_url": srv.URL}, "tok", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err == nil {
		t.Fatal("expected error on 500 response")
	}
}
