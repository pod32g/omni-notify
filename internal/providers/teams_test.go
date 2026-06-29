package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/pod32g/omni-notify/internal/models"
)

func TestNewTeamsSend(t *testing.T) {
	srv, body := captureServer(t, http.StatusOK)
	p, err := newTeams(srv.Client(), nil, srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err != nil {
		t.Fatal(err)
	}
	var card teamsCard
	if err := json.Unmarshal(*body, &card); err != nil {
		t.Fatalf("invalid teams payload: %v\n%s", err, *body)
	}
	if card.Type != "MessageCard" {
		t.Fatalf("@type = %q, want MessageCard", card.Type)
	}
	if !strings.Contains(card.Title, "Pi-hole Down") {
		t.Fatalf("title missing event: %q", card.Title)
	}
	if card.Summary == "" {
		t.Fatalf("summary must be non-empty")
	}
	if card.ThemeColor != "E74C3C" {
		t.Fatalf("themeColor = %q, want E74C3C (uppercase 6-hex, no #)", card.ThemeColor)
	}
}

func TestNewTeamsURLConfigOverride(t *testing.T) {
	srv, body := captureServer(t, http.StatusOK)
	cfg := map[string]any{"url": srv.URL}
	p, err := newTeams(srv.Client(), cfg, "https://outlook.office.com/webhook/real", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err != nil {
		t.Fatal(err)
	}
	var card teamsCard
	if err := json.Unmarshal(*body, &card); err != nil {
		t.Fatalf("invalid teams payload: %v\n%s", err, *body)
	}
	if card.Type != "MessageCard" {
		t.Fatalf("override target not used or bad payload: %+v", card)
	}
}

func TestNewTeamsRequiresSecret(t *testing.T) {
	if _, err := newTeams(http.DefaultClient, nil, "", true); err == nil {
		t.Fatal("expected error for missing webhook URL")
	}
}

func TestNewTeamsRejectsBadURL(t *testing.T) {
	if _, err := newTeams(http.DefaultClient, nil, "ftp://host/x", true); err == nil {
		t.Fatal("expected error for non-http URL")
	}
}

func TestNewTeamsNon2xxIsError(t *testing.T) {
	srv, _ := captureServer(t, http.StatusInternalServerError)
	p, _ := newTeams(srv.Client(), nil, srv.URL, true)
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err == nil {
		t.Fatal("expected error on 500 response")
	}
}
