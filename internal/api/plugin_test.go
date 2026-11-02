package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pod32g/omni-notify/internal/api"
)

func TestPluginRoutesAuthenticateThemselves(t *testing.T) {
	var sawPath, sawAuth string
	plugin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath, sawAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "plugin")
	})
	srv := api.NewServer(nil, nil, nil, nil, nil, nil, api.Config{
		Tokens:  []string{token},
		Plugins: map[string]http.Handler{"oncall": plugin},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest("GET", ts.URL+"/oncall/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer a-person")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "plugin" || sawPath != "/api/v1/me" || sawAuth != "Bearer a-person" {
		t.Fatalf("got %d %q, plugin saw %q %q", resp.StatusCode, body, sawPath, sawAuth)
	}
	// Notify's own API still needs an admin token.
	resp, err = http.Get(ts.URL + "/api/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("core API without a token: %d", resp.StatusCode)
	}
}
