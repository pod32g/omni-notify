package api_test

import (
	"bytes"
	"crypto/rand"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pod32g/omni-notify/internal/api"
	"github.com/pod32g/omni-notify/internal/clock"
	"github.com/pod32g/omni-notify/internal/metrics"
	"github.com/pod32g/omni-notify/internal/notifier"
	"github.com/pod32g/omni-notify/internal/providers"
	"github.com/pod32g/omni-notify/internal/storage"
	"github.com/prometheus/client_golang/prometheus"
)

// newServerWithLogger builds an API server whose access logs are captured into a
// buffer at the given level, so tests can assert what is and isn't logged.
func newServerWithLogger(t *testing.T, level slog.Level) (*httptest.Server, *bytes.Buffer) {
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

	reg := providers.NewDefault(nil, false)
	m := metrics.New()
	promReg := prometheus.NewRegistry()
	m.MustRegister(promReg)
	n := notifier.New(store, reg, m, clk, nil, notifier.Config{})

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level}))
	srv := api.NewServer(store, n, reg, m, promReg, logger, api.Config{
		Tokens:       []string{token},
		MaxBodyBytes: 1 << 20,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, &buf
}

func TestProbePathsNotAccessLoggedAtInfo(t *testing.T) {
	ts, buf := newServerWithLogger(t, slog.LevelInfo)

	for _, p := range []string{"/healthz", "/metrics"} {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	if strings.Contains(buf.String(), `"path":"/healthz"`) {
		t.Errorf("/healthz should not be access-logged at info level; log:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), `"path":"/metrics"`) {
		t.Errorf("/metrics should not be access-logged at info level; log:\n%s", buf.String())
	}
}

func TestNormalRequestAccessLoggedAtInfo(t *testing.T) {
	ts, buf := newServerWithLogger(t, slog.LevelInfo)

	// Unauthenticated API call: returns 401 but still passes through logMiddleware.
	resp, err := http.Get(ts.URL + "/api/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	out := buf.String()
	if !strings.Contains(out, `"msg":"request"`) || !strings.Contains(out, `"path":"/api/v1/events"`) {
		t.Errorf("normal request should be access-logged at info; log:\n%s", out)
	}
}

func TestUIPathsNotAccessLoggedAtInfo(t *testing.T) {
	ts, buf := newServerWithLogger(t, slog.LevelInfo)

	for _, p := range []string{"/", "/assets/app.js", "/assets/styles.css"} {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	// Serving the dashboard shell and its static assets must not be access-logged
	// at the default info level.
	if strings.Contains(buf.String(), `"msg":"request"`) {
		t.Errorf("UI shell/assets should not be access-logged at info; log:\n%s", buf.String())
	}
}

func TestProbePathsAccessLoggedAtDebug(t *testing.T) {
	ts, buf := newServerWithLogger(t, slog.LevelDebug)

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if !strings.Contains(buf.String(), `"path":"/healthz"`) {
		t.Errorf("/healthz should be access-logged at debug level; log:\n%s", buf.String())
	}
}
