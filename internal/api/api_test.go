package api_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pod32g/omni-notify/internal/api"
	"github.com/pod32g/omni-notify/internal/clock"
	"github.com/pod32g/omni-notify/internal/metrics"
	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/notifier"
	"github.com/pod32g/omni-notify/internal/providers"
	"github.com/pod32g/omni-notify/internal/storage"
	"github.com/prometheus/client_golang/prometheus"
)

const token = "test-token"

type okProvider struct{}

func (okProvider) Send(context.Context, models.NotificationMessage) error { return nil }

func newTestServer(t *testing.T, maxBody int64) (*httptest.Server, *storage.Store) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 6, 28, 20, 0, 0, 0, time.UTC))
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	cipher, err := storage.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(filepath.Join(t.TempDir(), "api.db"), cipher, clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	// Real providers (with SSRF guard, allowPrivate=false) plus a stub for tests
	// that just need a provider that always succeeds.
	reg := providers.NewDefault(nil, false)
	reg.Register("stub", func(map[string]any, string) (providers.Provider, error) { return okProvider{}, nil })

	m := metrics.New()
	promReg := prometheus.NewRegistry()
	m.MustRegister(promReg)
	n := notifier.New(store, reg, m, clk, nil, notifier.Config{})

	srv := api.NewServer(store, n, reg, m, promReg, nil, api.Config{
		Tokens:       []string{token},
		MaxBodyBytes: maxBody,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, store
}

func do(t *testing.T, ts *httptest.Server, method, path, body string, auth bool) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

const eventBody = `{"event_id":"pihole-down","type":"alert","source":"homelab",
	"status":"firing","severity":"critical","title":"Pi-hole Down",
	"labels":{"service":"pihole"},"timestamp":"2026-06-28T20:00:00Z","fingerprint":"fp-1"}`

func TestAuthRequired(t *testing.T) {
	ts, _ := newTestServer(t, 1<<20)
	if resp, _ := do(t, ts, http.MethodPost, "/api/v1/events", eventBody, false); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-auth status = %d, want 401", resp.StatusCode)
	}
	if resp, _ := do(t, ts, http.MethodGet, "/api/v1/events", "", true); resp.StatusCode != http.StatusOK {
		t.Fatalf("auth status = %d, want 200", resp.StatusCode)
	}
}

func TestHealthAndMetricsNoAuth(t *testing.T) {
	ts, _ := newTestServer(t, 1<<20)
	if resp, _ := do(t, ts, http.MethodGet, "/healthz", "", false); resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}
	resp, body := do(t, ts, http.MethodGet, "/metrics", "", false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics = %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "omni_notify_") {
		t.Fatalf("metrics output missing omni_notify_ series")
	}
}

func TestIngestFlow(t *testing.T) {
	ts, store := newTestServer(t, 1<<20)

	resp, body := do(t, ts, http.MethodPost, "/api/v1/events", eventBody, true)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("ingest status = %d: %s", resp.StatusCode, body)
	}
	var res notifier.ProcessResult
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	if res.Fingerprint != "fp-1" {
		t.Fatalf("fingerprint = %q", res.Fingerprint)
	}

	// Event is listable and fetchable.
	_, listBody := do(t, ts, http.MethodGet, "/api/v1/events", "", true)
	if !strings.Contains(string(listBody), "pihole-down") {
		t.Fatalf("event not listed: %s", listBody)
	}
	_, byID := do(t, ts, http.MethodGet, "/api/v1/events/1", "", true)
	if !strings.Contains(string(byID), "Pi-hole Down") {
		t.Fatalf("event by id missing: %s", byID)
	}
	if resp, _ := do(t, ts, http.MethodGet, "/api/v1/events/9999", "", true); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing event status = %d, want 404", resp.StatusCode)
	}

	// State recorded and active.
	_, stateBody := do(t, ts, http.MethodGet, "/api/v1/states/fp-1", "", true)
	if !strings.Contains(string(stateBody), `"active":true`) {
		t.Fatalf("state not active: %s", stateBody)
	}
	_ = store
}

func TestIngestValidation(t *testing.T) {
	ts, _ := newTestServer(t, 1<<20)
	if resp, _ := do(t, ts, http.MethodPost, "/api/v1/events", `{bad json`, true); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad json status = %d, want 400", resp.StatusCode)
	}
	if resp, _ := do(t, ts, http.MethodPost, "/api/v1/events", `{"type":"alert"}`, true); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing fields status = %d, want 400", resp.StatusCode)
	}
}

func TestProviderSecretMasking(t *testing.T) {
	ts, _ := newTestServer(t, 1<<20)
	create := `{"name":"d","kind":"stub","secret":"super-secret-url","config":{"username":"bot"}}`
	resp, body := do(t, ts, http.MethodPost, "/api/v1/providers", create, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create provider = %d: %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "super-secret-url") {
		t.Fatalf("secret leaked in create response: %s", body)
	}
	if !strings.Contains(string(body), `"has_secret":true`) {
		t.Fatalf("has_secret not reported: %s", body)
	}
	_, listBody := do(t, ts, http.MethodGet, "/api/v1/providers", "", true)
	if strings.Contains(string(listBody), "super-secret-url") {
		t.Fatalf("secret leaked in list: %s", listBody)
	}
	// Unknown kind rejected.
	if resp, _ := do(t, ts, http.MethodPost, "/api/v1/providers", `{"name":"x","kind":"nope"}`, true); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown kind status = %d, want 400", resp.StatusCode)
	}
}

func TestRouteValidation(t *testing.T) {
	ts, _ := newTestServer(t, 1<<20)
	// Route referencing unknown provider is rejected.
	if resp, _ := do(t, ts, http.MethodPost, "/api/v1/routes", `{"name":"r","providers":["ghost"]}`, true); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown provider route = %d, want 400", resp.StatusCode)
	}
	// Create provider then route.
	do(t, ts, http.MethodPost, "/api/v1/providers", `{"name":"d","kind":"stub"}`, true)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/routes",
		`{"name":"r","match":{"severity":"critical"},"providers":["d"],"repeat_interval":"1h"}`, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create route = %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"1h0m0s"`) {
		t.Fatalf("repeat_interval not round-tripped: %s", body)
	}
}

func TestProviderCreateConflict(t *testing.T) {
	ts, _ := newTestServer(t, 1<<20)
	body := `{"name":"d","kind":"stub","secret":"s1"}`
	if resp, _ := do(t, ts, http.MethodPost, "/api/v1/providers", body, true); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create = %d, want 201", resp.StatusCode)
	}
	if resp, _ := do(t, ts, http.MethodPost, "/api/v1/providers", body, true); resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate create = %d, want 409", resp.StatusCode)
	}
}

func TestProviderPutReplaceAndPatch(t *testing.T) {
	ts, _ := newTestServer(t, 1<<20)
	do(t, ts, http.MethodPost, "/api/v1/providers", `{"name":"d","kind":"stub","secret":"s1","config":{"a":"1"}}`, true)

	// PUT replaces config wholesale; omitted secret is preserved.
	resp, body := do(t, ts, http.MethodPut, "/api/v1/providers/d", `{"kind":"stub","config":{"b":"2"}}`, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d: %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), `"a"`) || !strings.Contains(string(body), `"b"`) {
		t.Fatalf("PUT did not replace config: %s", body)
	}
	if !strings.Contains(string(body), `"has_secret":true`) {
		t.Fatalf("PUT lost the preserved secret: %s", body)
	}

	// PATCH changes only the provided field.
	resp, body = do(t, ts, http.MethodPatch, "/api/v1/providers/d", `{"enabled":false}`, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"enabled":false`) || !strings.Contains(string(body), `"b"`) {
		t.Fatalf("PATCH should disable but keep config: %s", body)
	}

	// PATCH on a missing provider is 404.
	if resp, _ := do(t, ts, http.MethodPatch, "/api/v1/providers/ghost", `{"enabled":true}`, true); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("PATCH missing = %d, want 404", resp.StatusCode)
	}
}

func TestPutCreatesReturns201(t *testing.T) {
	ts, _ := newTestServer(t, 1<<20)
	// PUT to a non-existent name creates it -> 201.
	if resp, b := do(t, ts, http.MethodPut, "/api/v1/providers/new", `{"kind":"stub"}`, true); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT create provider = %d, want 201: %s", resp.StatusCode, b)
	}
	// PUT again replaces it -> 200.
	if resp, _ := do(t, ts, http.MethodPut, "/api/v1/providers/new", `{"kind":"stub"}`, true); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT replace provider, want 200, got %d", resp.StatusCode)
	}
	// Same for routes.
	if resp, b := do(t, ts, http.MethodPut, "/api/v1/routes/nr", `{"providers":["new"]}`, true); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT create route = %d, want 201: %s", resp.StatusCode, b)
	}
}

func TestRouteCreateConflictAndUpdate(t *testing.T) {
	ts, _ := newTestServer(t, 1<<20)
	do(t, ts, http.MethodPost, "/api/v1/providers", `{"name":"d","kind":"stub"}`, true)

	body := `{"name":"r","providers":["d"],"priority":3}`
	if resp, _ := do(t, ts, http.MethodPost, "/api/v1/routes", body, true); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create route = %d, want 201", resp.StatusCode)
	}
	if resp, _ := do(t, ts, http.MethodPost, "/api/v1/routes", body, true); resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate route = %d, want 409", resp.StatusCode)
	}
	// PUT replace.
	if resp, b := do(t, ts, http.MethodPut, "/api/v1/routes/r", `{"providers":["d"],"priority":7,"stop_processing":true}`, true); resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"priority":7`) {
		t.Fatalf("PUT route = %d: %s", resp.StatusCode, b)
	}
	// PATCH priority only.
	if resp, b := do(t, ts, http.MethodPatch, "/api/v1/routes/r", `{"priority":9}`, true); resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"priority":9`) {
		t.Fatalf("PATCH route = %d: %s", resp.StatusCode, b)
	}
}

func TestLegacyStatusMigratedOnIngest(t *testing.T) {
	ts, _ := newTestServer(t, 1<<20)
	body := `{"event_id":"x","type":"alert","source":"s","title":"t","timestamp":"2026-06-28T20:00:00Z","status":"warning"}`
	if resp, b := do(t, ts, http.MethodPost, "/api/v1/events", body, true); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("ingest legacy status = %d: %s", resp.StatusCode, b)
	}
	_, ev := do(t, ts, http.MethodGet, "/api/v1/events/1", "", true)
	if !strings.Contains(string(ev), `"severity":"warning"`) {
		t.Fatalf("legacy status not migrated to severity: %s", ev)
	}
	if strings.Contains(string(ev), `"status":"warning"`) {
		t.Fatalf("legacy status should be cleared: %s", ev)
	}
}

func TestProviderBuildValidation(t *testing.T) {
	ts, _ := newTestServer(t, 1<<20) // allowPrivate=false in the test registry
	cases := []struct {
		name string
		body string
		want int
	}{
		{"bad scheme", `{"name":"a","kind":"webhook","secret":"file:///etc/passwd"}`, http.StatusBadRequest},
		{"private host", `{"name":"b","kind":"webhook","secret":"http://127.0.0.1/x"}`, http.StatusBadRequest},
		{"smtp missing host", `{"name":"c","kind":"smtp","secret":"pw","config":{"from":"a@b","to":["c@d"]}}`, http.StatusBadRequest},
		{"valid public webhook", `{"name":"d","kind":"webhook","secret":"https://example.com/hook"}`, http.StatusCreated},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, body := do(t, ts, http.MethodPost, "/api/v1/providers", c.body, true)
			if resp.StatusCode != c.want {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, c.want, body)
			}
		})
	}
}

func TestTestEndpoint(t *testing.T) {
	ts, _ := newTestServer(t, 1<<20)
	do(t, ts, http.MethodPost, "/api/v1/providers", `{"name":"d","kind":"stub"}`, true)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/test", `{"provider":"d"}`, true)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("test endpoint = %d: %s", resp.StatusCode, body)
	}
	if resp, _ := do(t, ts, http.MethodPost, "/api/v1/test", `{"provider":"missing"}`, true); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("test missing provider = %d, want 404", resp.StatusCode)
	}
}

func TestMaxBodyLimit(t *testing.T) {
	ts, _ := newTestServer(t, 64) // tiny limit
	big := `{"event_id":"` + strings.Repeat("x", 500) + `","type":"a","source":"s","title":"t","timestamp":"2026-06-28T20:00:00Z"}`
	resp, _ := do(t, ts, http.MethodPost, "/api/v1/events", big, true)
	if resp.StatusCode != http.StatusRequestEntityTooLarge && resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized body status = %d, want 413/400", resp.StatusCode)
	}
}

func TestUnknownTokenRejected(t *testing.T) {
	ts, _ := newTestServer(t, 1<<20)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, _ := ts.Client().Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", resp.StatusCode)
	}
}
