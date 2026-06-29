package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pod32g/omni-notify/internal/models"
)

func TestNewNtfy_Send(t *testing.T) {
	var gotBody []byte
	var gotMethod, gotTitle, gotPriority, gotTags, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotTitle = r.Header.Get("Title")
		gotPriority = r.Header.Get("Priority")
		gotTags = r.Header.Get("Tags")
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := map[string]any{"priority": 4, "tags": "warning,skull", "token": "tok123"}
	p, err := newNtfy(srv.Client(), cfg, srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if !strings.Contains(gotTitle, "Pi-hole Down") {
		t.Fatalf("Title header missing event subject: %q", gotTitle)
	}
	if gotPriority != "4" {
		t.Fatalf("Priority header = %q, want 4", gotPriority)
	}
	if gotTags != "warning,skull" {
		t.Fatalf("Tags header = %q", gotTags)
	}
	if gotAuth != "Bearer tok123" {
		t.Fatalf("Authorization header = %q", gotAuth)
	}
	if len(gotBody) == 0 || !strings.Contains(string(gotBody), "not responding") {
		t.Fatalf("body missing event content: %q", gotBody)
	}
}

func TestNewNtfy_MinimalNoOptionalHeaders(t *testing.T) {
	var gotPriority, gotTags, gotAuth, gotTitle string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPriority = r.Header.Get("Priority")
		gotTags = r.Header.Get("Tags")
		gotAuth = r.Header.Get("Authorization")
		gotTitle = r.Header.Get("Title")
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	p, err := newNtfy(srv.Client(), nil, srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err != nil {
		t.Fatal(err)
	}
	if gotTitle == "" {
		t.Fatal("expected Title header to be set")
	}
	if gotPriority != "" || gotTags != "" || gotAuth != "" {
		t.Fatalf("expected no optional headers, got priority=%q tags=%q auth=%q", gotPriority, gotTags, gotAuth)
	}
}

func TestNewNtfy_RequiresSecret(t *testing.T) {
	if _, err := newNtfy(http.DefaultClient, nil, "", true); err == nil {
		t.Fatal("expected error for missing topic URL")
	}
}

func TestNewNtfy_InvalidPriority(t *testing.T) {
	if _, err := newNtfy(http.DefaultClient, map[string]any{"priority": 9}, "https://ntfy.sh/x", true); err == nil {
		t.Fatal("expected error for out-of-range priority")
	}
}

func TestNewNtfy_Non2xxIsError(t *testing.T) {
	srv, _ := captureServer(t, http.StatusInternalServerError)
	p, _ := newNtfy(srv.Client(), nil, srv.URL, true)
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err == nil {
		t.Fatal("expected error on 500 response")
	}
}
