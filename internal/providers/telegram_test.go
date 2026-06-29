package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pod32g/omni-notify/internal/models"
)

func TestNewTelegramSend(t *testing.T) {
	srv, body := captureServer(t, http.StatusOK)
	cfg := map[string]any{
		"chat_id":    "12345",
		"parse_mode": "Markdown",
		"api_base":   srv.URL,
	}
	p, err := newTelegram(srv.Client(), cfg, "bot-token", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err != nil {
		t.Fatal(err)
	}
	var payload telegramPayload
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
	p, err := newTelegram(srv.Client(), cfg, "tok123", true)
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
	if _, err := newTelegram(http.DefaultClient, map[string]any{}, "tok", true); err == nil {
		t.Fatal("expected error for missing chat_id")
	}
	// Missing secret (bot token).
	if _, err := newTelegram(http.DefaultClient, map[string]any{"chat_id": "1"}, "", true); err == nil {
		t.Fatal("expected error for missing bot token")
	}
}
