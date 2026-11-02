package api_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pod32g/omni-notify/internal/api"
	"github.com/pod32g/omni-notify/internal/clock"
	"github.com/pod32g/omni-notify/internal/config"
	"github.com/pod32g/omni-notify/internal/metrics"
	"github.com/pod32g/omni-notify/internal/notifier"
	"github.com/pod32g/omni-notify/internal/providers"
	"github.com/pod32g/omni-notify/internal/storage"
	"github.com/pod32g/omni-notify/internal/voice"
	"github.com/prometheus/client_golang/prometheus"
)

const claudeToken = "claude-agent-token"

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

type voiceEnv struct {
	ts    *httptest.Server
	svc   *voice.Service
	clk   *clock.Fake
	store *storage.Store
}

func newVoiceServer(t *testing.T) *voiceEnv {
	t.Helper()
	cfg, err := config.Parse([]byte(`
security: {tokens: ["` + token + `"]}
voice:
  enabled: true
  agents:
    - id: claude
      display_name: Claude
      token_sha256: ["` + sha(claudeToken) + `"]
      allow_test: true
    - id: gpt
      display_name: GPT
      token_sha256: ["` + sha("gpt-token") + `"]
  users:
    - id: pod32g
      aliases: [me]
      timezone: America/Mexico_City
      allowed_agents: [claude, gpt]
`))
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 10, 26, 22, 45, 0, 0, time.UTC))
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cipher, _ := storage.NewCipher(key)
	store, err := storage.Open(filepath.Join(t.TempDir(), "v.db"), cipher, clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	m := metrics.New()
	promReg := prometheus.NewRegistry()
	m.MustRegister(promReg)
	reg := providers.NewDefault(nil, false)
	n := notifier.New(store, reg, m, clk, nil, notifier.Config{})
	svc := voice.NewService(cfg.Voice, store, m, voice.Options{Clock: clk, Fallback: n})
	reg.Register("voice_call", func(c map[string]any, _ string) (providers.Provider, error) { return voice.NewRouteProvider(svc, c) })
	srv := api.NewServer(store, n, reg, m, promReg, nil, api.Config{Tokens: []string{token}, MaxBodyBytes: 1 << 20, Voice: svc})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &voiceEnv{ts: ts, svc: svc, clk: clk, store: store}
}

func (e *voiceEnv) do(t *testing.T, method, path, bearer, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *voiceEnv) enroll(t *testing.T) string {
	t.Helper()
	code, body := e.do(t, "POST", "/api/v1/devices/enrollments", token, `{"user":"me"}`)
	if code != 201 {
		t.Fatalf("enrollment: %d %v", code, body)
	}
	code, body = e.do(t, "POST", "/api/v1/devices/enroll", "",
		`{"code":"`+body["code"].(string)+`","name":"Suzuka","bundle_id":"dev.pod32g.OmniNotify","apns_environment":"development"}`)
	if code != 201 {
		t.Fatalf("enroll: %d %v", code, body)
	}
	return body["device_token"].(string)
}

func TestVoiceAPIPrincipals(t *testing.T) {
	e := newVoiceServer(t)
	dev := e.enroll(t)

	// Agent and device credentials are confined to the voice API.
	for _, tok := range []string{claudeToken, dev} {
		if code, _ := e.do(t, "GET", "/api/v1/events", tok, ""); code != http.StatusForbidden {
			t.Errorf("%s… on /events: %d, want 403", tok[:4], code)
		}
		if code, _ := e.do(t, "GET", "/api/v1/devices", tok, ""); code != http.StatusForbidden {
			t.Errorf("%s… on /devices: %d, want 403", tok[:4], code)
		}
	}
	if code, _ := e.do(t, "POST", "/api/v1/calls", "nonsense", `{}`); code != http.StatusUnauthorized {
		t.Errorf("unknown token: %d", code)
	}
	// The agent comes from the credential; a mismatching claim is refused.
	if code, b := e.do(t, "POST", "/api/v1/calls", claudeToken, `{"recipient":"me","agent":"gpt","message":"hi"}`); code != 403 {
		t.Errorf("impersonation: %d %v", code, b)
	}
	// Admin must name the agent, and the call is simulated.
	if code, _ := e.do(t, "POST", "/api/v1/calls", token, `{"recipient":"me","message":"hi"}`); code != 400 {
		t.Errorf("admin without agent: %d", code)
	}
	code, b := e.do(t, "POST", "/api/v1/calls", token, `{"recipient":"me","agent":"claude","message":"hi","test":true}`)
	call := b["call"].(map[string]any)
	if code != 201 || call["simulated"] != true || call["caller_name"] != "Claude (test)" {
		t.Errorf("admin as claude: %d %v", code, b)
	}
	// Devices cannot place calls; agents cannot report call state.
	if code, _ := e.do(t, "POST", "/api/v1/calls", dev, `{"recipient":"me","message":"hi"}`); code != 403 {
		t.Errorf("device placing a call: %d", code)
	}
	if code, _ := e.do(t, "POST", "/api/v1/calls/"+call["id"].(string)+"/state", claudeToken, `{"state":"answered"}`); code != 403 {
		t.Errorf("agent reporting state: %d", code)
	}
	// gpt cannot see claude's call.
	if code, _ := e.do(t, "GET", "/api/v1/calls/"+call["id"].(string), "gpt-token", ""); code != 404 {
		t.Errorf("gpt reading claude's call: %d", code)
	}
	// Unknown fields are rejected rather than ignored.
	if code, _ := e.do(t, "POST", "/api/v1/calls", claudeToken, `{"recipient":"me","message":"hi","priority":"CRITICAL"}`); code != 400 {
		t.Errorf("unknown field: %d", code)
	}
}

func TestVoiceAPISnoozeFlow(t *testing.T) {
	e := newVoiceServer(t)
	dev := e.enroll(t)

	code, b := e.do(t, "POST", "/api/v1/calls", claudeToken, `{"recipient":"me","type":"reminder","severity":"info",
		"delivery_channel":"VOICE_CALL","message":"Remember to lend Claude your phone for testing.","idempotency_key":"r1"}`)
	if code != 201 {
		t.Fatalf("create: %d %v", code, b)
	}
	id := b["call"].(map[string]any)["id"].(string)
	if code, b := e.do(t, "POST", "/api/v1/calls", claudeToken, `{"recipient":"me","message":"again","idempotency_key":"r1"}`); code != 200 || b["deduplicated"] != true {
		t.Fatalf("replay: %d %v", code, b)
	}

	e.svc.Tick(context.Background())
	// No APNs in this server: the phone finds the call by polling.
	_, b = e.do(t, "GET", "/api/v1/devices/self/ringing", dev, "")
	calls := b["calls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["call_id"] != id || calls[0].(map[string]any)["caller_name"] != "Claude" {
		t.Fatalf("ringing = %v", b)
	}
	code, b = e.do(t, "GET", "/api/v1/calls/"+id, dev, "")
	if code != 200 || !strings.Contains(b["greeting"].(map[string]any)["text"].(string), "reminder from Claude") {
		t.Fatalf("device view: %d %v", code, b)
	}
	for _, st := range []string{`{"state":"answered"}`, `{"state":"active","first_audio_ms":640}`} {
		if code, b := e.do(t, "POST", "/api/v1/calls/"+id+"/state", dev, st); code != 200 {
			t.Fatalf("%s: %d %v", st, code, b)
		}
	}
	code, b = e.do(t, "POST", "/api/v1/calls/"+id+"/turns", dev, `{"seq":1,"transcript":"Give me another ten minutes.","stt_ms":420}`)
	if code != 200 || b["end_call"] != true || b["action"].(map[string]any)["status"] != "persisted" {
		t.Fatalf("turn: %d %v", code, b)
	}
	next := b["action"].(map[string]any)["call_id"].(string)
	if code, b := e.do(t, "POST", "/api/v1/calls/"+id+"/state", dev, `{"state":"completed"}`); code != 200 {
		t.Fatalf("complete: %d %v", code, b)
	}

	// The agent sees the outcome and the scheduled follow-up.
	code, b = e.do(t, "GET", "/api/v1/calls/"+id, claudeToken, "")
	if code != 200 || b["call"].(map[string]any)["outcome"] != "snoozed" || len(b["log"].([]any)) < 5 {
		t.Fatalf("status: %d %v", code, b)
	}
	code, b = e.do(t, "GET", "/api/v1/calls?state=scheduled", claudeToken, "")
	if code != 200 || b["count"].(float64) != 1 || b["calls"].([]any)[0].(map[string]any)["id"] != next {
		t.Fatalf("scheduled list: %d %v", code, b)
	}
	// It can cancel it.
	if code, b := e.do(t, "POST", "/api/v1/calls/"+next+"/cancel", claudeToken, ""); code != 200 {
		t.Fatalf("cancel: %d %v", code, b)
	}
	if code, _ := e.do(t, "POST", "/api/v1/calls/"+next+"/cancel", claudeToken, ""); code != 409 {
		t.Fatalf("cancel twice: %d", code)
	}
	// Metrics are exported.
	resp, err := http.Get(e.ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 1<<16)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	for _, m := range []string{"omni_notify_voice_calls_total", "omni_notify_voice_calls_answered_total 1",
		"omni_notify_voice_first_audio_latency_seconds_count 1", "omni_notify_voice_turns_total{intent=\"snooze\"} 1",
		"omni_notify_voice_scheduled_calls_total{kind=\"snooze\"} 1"} {
		if !strings.Contains(sb.String(), m) {
			t.Errorf("metrics missing %q", m)
		}
	}
}

func TestVoiceProviderRoutesEventsToCalls(t *testing.T) {
	e := newVoiceServer(t)
	e.enroll(t)
	if code, b := e.do(t, "POST", "/api/v1/providers", token,
		`{"name":"call-me","kind":"voice_call","config":{"recipient":"me","agent":"claude"}}`); code != 201 {
		t.Fatalf("provider: %d %v", code, b)
	}
	if code, b := e.do(t, "POST", "/api/v1/routes", token,
		`{"name":"phone","match":{"labels.call":"yes"},"providers":["call-me"]}`); code != 201 {
		t.Fatalf("route: %d %v", code, b)
	}
	if code, b := e.do(t, "POST", "/api/v1/events", token, `{"event_id":"e1","type":"alert","source":"homelab",
		"severity":"warning","title":"Disk almost full","summary":"cold2 is at 97 percent","labels":{"call":"yes"},
		"timestamp":"2026-10-26T22:45:00Z"}`); code != 202 {
		t.Fatalf("event: %d %v", code, b)
	}
	// The delivery engine is not running in this test: deliver by hand.
	ctx, cancel := context.WithCancel(context.Background())
	n := notifier.New(e.store, func() *providers.Registry {
		r := providers.NewDefault(nil, false)
		r.Register("voice_call", func(c map[string]any, _ string) (providers.Provider, error) { return voice.NewRouteProvider(e.svc, c) })
		return r
	}(), metrics.New(), e.clk, nil, notifier.Config{PollInterval: 10 * time.Millisecond})
	if err := n.Start(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var calls []any
	for time.Now().Before(deadline) {
		_, b := e.do(t, "GET", "/api/v1/calls", token, "")
		if calls = b["calls"].([]any); len(calls) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	n.Stop()
	if len(calls) != 1 {
		t.Fatalf("calls = %v", calls)
	}
	c := calls[0].(map[string]any)
	if c["origin"] != "route" || c["message"] != "cold2 is at 97 percent" || c["severity"] != "warning" || c["agent"] != "claude" {
		t.Fatalf("routed call = %v", c)
	}
}
