package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/pod32g/omni-notify/internal/models"
)

func TestNewGoogleChatSend(t *testing.T) {
	srv, body := captureServer(t, http.StatusOK)
	p, err := newGoogleChat(srv.Client(), nil, srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err != nil {
		t.Fatal(err)
	}
	var payload googleChatPayload
	if err := json.Unmarshal(*body, &payload); err != nil {
		t.Fatalf("invalid googlechat payload: %v\n%s", err, *body)
	}
	if payload.Text == "" {
		t.Fatal("expected non-empty text")
	}
	if !strings.Contains(payload.Text, "Pi-hole Down") {
		t.Fatalf("text missing event title: %q", payload.Text)
	}
}

func TestNewGoogleChatConfigURLOverride(t *testing.T) {
	srv, body := captureServer(t, http.StatusOK)
	// secret is a public placeholder; the config url override targets the test server.
	p, err := newGoogleChat(srv.Client(), map[string]any{"url": srv.URL}, "https://chat.googleapis.com/v1/spaces/x", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err != nil {
		t.Fatal(err)
	}
	var payload googleChatPayload
	if err := json.Unmarshal(*body, &payload); err != nil {
		t.Fatalf("invalid googlechat payload: %v\n%s", err, *body)
	}
	if !strings.Contains(payload.Text, "Pi-hole Down") {
		t.Fatalf("text missing event title: %q", payload.Text)
	}
}

func TestNewGoogleChatRequiresSecret(t *testing.T) {
	if _, err := newGoogleChat(http.DefaultClient, nil, "", true); err == nil {
		t.Fatal("expected error for missing webhook URL")
	}
}

func TestNewGoogleChatRejectsBadURL(t *testing.T) {
	if _, err := newGoogleChat(http.DefaultClient, nil, "ftp://host/x", true); err == nil {
		t.Fatal("expected error for non-http URL")
	}
}

func TestNewGoogleChatNon2xxIsError(t *testing.T) {
	srv, _ := captureServer(t, http.StatusInternalServerError)
	p, _ := newGoogleChat(srv.Client(), nil, srv.URL, true)
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err == nil {
		t.Fatal("expected error on 500 response")
	}
}
