package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pod32g/omni-notify/internal/models"
)

func TestNewGotify_Send(t *testing.T) {
	var gotPath, gotKey string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-Gotify-Key")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	p, err := newGotify(srv.Client(), map[string]any{"url": srv.URL, "priority": 8}, "tok123", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/message" {
		t.Fatalf("unexpected path %q", gotPath)
	}
	if gotKey != "tok123" {
		t.Fatalf("X-Gotify-Key header = %q, want tok123", gotKey)
	}
	var payload gotifyPayload
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("invalid gotify payload: %v\n%s", err, gotBody)
	}
	if !strings.Contains(payload.Title, "Pi-hole Down") {
		t.Fatalf("title missing: %q", payload.Title)
	}
	if !strings.Contains(payload.Message, "not responding") {
		t.Fatalf("message missing body: %q", payload.Message)
	}
	if payload.Priority != 8 {
		t.Fatalf("priority = %d, want 8", payload.Priority)
	}
}

func TestNewGotify_DefaultPriority(t *testing.T) {
	srv, _ := captureServer(t, http.StatusOK)
	p, err := newGotify(srv.Client(), map[string]any{"url": srv.URL}, "tok", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.(*gotify).priority; got != 5 {
		t.Fatalf("default priority = %d, want 5", got)
	}
}

func TestNewGotify_RequiresToken(t *testing.T) {
	if _, err := newGotify(http.DefaultClient, map[string]any{"url": "https://gotify.example.com"}, "", true); err == nil {
		t.Fatal("expected error for missing token")
	}
}

func TestNewGotify_RequiresURL(t *testing.T) {
	if _, err := newGotify(http.DefaultClient, nil, "tok", true); err == nil {
		t.Fatal("expected error for missing url")
	}
}

func TestNewGotify_Non2xxIsError(t *testing.T) {
	srv, _ := captureServer(t, http.StatusInternalServerError)
	p, _ := newGotify(srv.Client(), map[string]any{"url": srv.URL}, "tok", true)
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err == nil {
		t.Fatal("expected error on 500 response")
	}
}
