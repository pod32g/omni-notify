package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/pod32g/omni-notify/internal/models"
)

func TestNewTwilioSend(t *testing.T) {
	var gotPath, gotAuth, gotCT string
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		_ = r.ParseForm()
		gotForm = r.PostForm
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)

	cfg := map[string]any{
		"account_sid": "AC123",
		"from":        "+15550001111",
		"to":          "+15552223333",
		"api_base":    srv.URL,
	}
	p, err := newTwilio(srv.Client(), cfg, "tok3n", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(gotPath, "/Accounts/AC123/Messages.json") {
		t.Fatalf("unexpected request path: %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Fatalf("expected Basic auth header, got %q", gotAuth)
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Fatalf("unexpected content type: %q", gotCT)
	}
	if gotForm.Get("From") != "+15550001111" {
		t.Fatalf("From not sent: %q", gotForm.Get("From"))
	}
	if gotForm.Get("To") != "+15552223333" {
		t.Fatalf("To not sent: %q", gotForm.Get("To"))
	}
	if !strings.Contains(gotForm.Get("Body"), "Pi-hole Down") {
		t.Fatalf("Body missing subject: %q", gotForm.Get("Body"))
	}
}

func TestNewTwilioRequiresFields(t *testing.T) {
	base := map[string]any{
		"account_sid": "AC123",
		"from":        "+15550001111",
		"to":          "+15552223333",
	}
	// Missing secret.
	if _, err := newTwilio(http.DefaultClient, base, "", true); err == nil {
		t.Error("expected error for missing auth token")
	}
	// Missing 'to'.
	noTo := map[string]any{"account_sid": "AC123", "from": "+15550001111"}
	if _, err := newTwilio(http.DefaultClient, noTo, "tok", true); err == nil {
		t.Error("expected error for missing to")
	}
	// Missing account_sid.
	noSID := map[string]any{"from": "+15550001111", "to": "+15552223333"}
	if _, err := newTwilio(http.DefaultClient, noSID, "tok", true); err == nil {
		t.Error("expected error for missing account_sid")
	}
}

func TestNewTwilioNon2xxIsError(t *testing.T) {
	srv, _ := captureServer(t, http.StatusInternalServerError)
	cfg := map[string]any{
		"account_sid": "AC123",
		"from":        "+15550001111",
		"to":          "+15552223333",
		"api_base":    srv.URL,
	}
	p, err := newTwilio(srv.Client(), cfg, "tok", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err == nil {
		t.Fatal("expected error on 500 response")
	}
}
