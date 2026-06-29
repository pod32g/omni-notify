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

func TestNewMatrix_Send(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotAuth   string
		gotBody   []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := map[string]any{"homeserver": srv.URL, "room_id": "!r:hs"}
	p, err := newMatrix(srv.Client(), cfg, "tok3n", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err != nil {
		t.Fatal(err)
	}

	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if !strings.Contains(gotPath, "/_matrix/client/v3/rooms/") {
		t.Errorf("path missing rooms prefix: %q", gotPath)
	}
	if !strings.Contains(gotPath, "/send/m.room.message/") {
		t.Errorf("path missing send segment: %q", gotPath)
	}
	if gotAuth != "Bearer tok3n" {
		t.Errorf("authorization = %q, want %q", gotAuth, "Bearer tok3n")
	}

	var payload matrixMessage
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("invalid matrix payload: %v\n%s", err, gotBody)
	}
	if payload.Msgtype != "m.text" {
		t.Errorf("msgtype = %q, want m.text", payload.Msgtype)
	}
	if !strings.Contains(payload.Body, "Pi-hole Down") {
		t.Errorf("body missing subject: %q", payload.Body)
	}
}

func TestNewMatrix_RequiresToken(t *testing.T) {
	cfg := map[string]any{"homeserver": "https://matrix.org", "room_id": "!r:hs"}
	if _, err := newMatrix(http.DefaultClient, cfg, "", true); err == nil {
		t.Fatal("expected error for missing access token")
	}
}

func TestNewMatrix_RequiresHomeserverAndRoom(t *testing.T) {
	if _, err := newMatrix(http.DefaultClient, map[string]any{"room_id": "!r:hs"}, "tok", true); err == nil {
		t.Error("expected error for missing homeserver")
	}
	if _, err := newMatrix(http.DefaultClient, map[string]any{"homeserver": "https://matrix.org"}, "tok", true); err == nil {
		t.Error("expected error for missing room_id")
	}
}

func TestNewMatrix_Non2xxIsError(t *testing.T) {
	srv, _ := captureServer(t, http.StatusForbidden)
	cfg := map[string]any{"homeserver": srv.URL, "room_id": "!r:hs"}
	p, err := newMatrix(srv.Client(), cfg, "tok", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: testEvent()}); err == nil {
		t.Fatal("expected error on 403 response")
	}
}
