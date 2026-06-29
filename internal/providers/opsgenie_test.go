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

func TestNewOpsgenie_FiringCreatesAlert(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	apiURL := srv.URL + "/v2/alerts"
	p, err := newOpsgenie(srv.Client(), map[string]any{"api_url": apiURL}, "k3y", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err != nil {
		t.Fatal(err)
	}

	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v2/alerts" {
		t.Fatalf("path = %q, want /v2/alerts", gotPath)
	}
	if gotAuth != "GenieKey k3y" {
		t.Fatalf("auth = %q, want GenieKey k3y", gotAuth)
	}
	var create opsgenieCreate
	if err := json.Unmarshal(gotBody, &create); err != nil {
		t.Fatalf("invalid create body: %v\n%s", err, gotBody)
	}
	if !strings.Contains(create.Message, "Pi-hole Down") {
		t.Fatalf("message missing title: %q", create.Message)
	}
	if create.Alias != "pihole-down" {
		t.Fatalf("alias = %q, want pihole-down (EventID fallback)", create.Alias)
	}
}

func TestNewOpsgenie_ResolvedClosesAlert(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	apiURL := srv.URL + "/v2/alerts"
	p, err := newOpsgenie(srv.Client(), map[string]any{"api_url": apiURL}, "k3y", true)
	if err != nil {
		t.Fatal(err)
	}
	ev := testEvent()
	ev.Status = models.StatusResolved
	ev.Fingerprint = "fp-123"
	if err := p.Send(context.Background(), models.NotificationMessage{Event: ev}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(gotPath, "/close") {
		t.Fatalf("path = %q, want suffix /close", gotPath)
	}
	if !strings.Contains(gotPath, "/fp-123/") {
		t.Fatalf("path = %q, want alias fp-123 in path", gotPath)
	}
}

func TestNewOpsgenie_RequiresSecret(t *testing.T) {
	if _, err := newOpsgenie(http.DefaultClient, nil, "", true); err == nil {
		t.Fatal("expected error for missing API key secret")
	}
}
