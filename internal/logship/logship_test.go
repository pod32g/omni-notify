package logship

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// capture records ingest requests received by a fake omni-logging server.
type capture struct {
	mu      sync.Mutex
	headers []http.Header
	lines   []map[string]any
}

func (c *capture) record(t *testing.T, h http.Header, body []byte) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.headers = append(c.headers, h.Clone())
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		ln := sc.Bytes()
		if len(bytes.TrimSpace(ln)) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(ln, &m); err != nil {
			t.Errorf("ingest line is not valid JSON: %q: %v", ln, err)
			continue
		}
		c.lines = append(c.lines, m)
	}
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.lines)
}

func (c *capture) snapshot() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]map[string]any, len(c.lines))
	copy(out, c.lines)
	return out
}

func (c *capture) lastHeader() http.Header {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.headers) == 0 {
		return nil
	}
	return c.headers[len(c.headers)-1]
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}

// newForwarder spins up a fake server + Forwarder, applying cfg overrides.
func newForwarder(t *testing.T, mutate func(*Config)) (*Forwarder, *capture) {
	t.Helper()
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cap.record(t, r.Header, body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := Config{
		Endpoint:      srv.URL,
		APIKey:        "test-key",
		Service:       "omni-notify",
		BatchSize:     2,
		BufferSize:    100,
		FlushInterval: time.Hour, // disabled unless a test overrides it
		Timeout:       time.Second,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	f, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = f.Shutdown(ctx)
	})
	return f, cap
}

func TestForwardsRecordsAsNDJSON(t *testing.T) {
	f, cap := newForwarder(t, nil) // BatchSize 2 -> flush after 2 records
	log := slog.New(f.Handler())

	log.Info("first")
	log.Error("second")

	waitFor(t, time.Second, func() bool { return cap.count() >= 2 })

	lines := cap.snapshot()
	if got := lines[0]["message"]; got != "first" {
		t.Errorf("message = %v, want first", got)
	}
	if got := lines[0]["level"]; got != "info" {
		t.Errorf("level = %v, want info", got)
	}
	if got := lines[1]["level"]; got != "error" {
		t.Errorf("level = %v, want error", got)
	}
	if got := lines[0]["service"]; got != "omni-notify" {
		t.Errorf("service = %v, want omni-notify", got)
	}
	if _, ok := lines[0]["timestamp"]; !ok {
		t.Errorf("timestamp field missing: %v", lines[0])
	}

	h := cap.lastHeader()
	if got := h.Get("X-Api-Key"); got != "test-key" {
		t.Errorf("X-Api-Key = %q, want test-key", got)
	}
	if ct := h.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson", ct)
	}
}

func TestFlushesPartialBatchOnInterval(t *testing.T) {
	f, cap := newForwarder(t, func(c *Config) {
		c.BatchSize = 100 // never size-flush
		c.FlushInterval = 30 * time.Millisecond
	})
	log := slog.New(f.Handler())

	log.Info("solo")

	waitFor(t, time.Second, func() bool { return cap.count() >= 1 })
	if got := cap.snapshot()[0]["message"]; got != "solo" {
		t.Errorf("message = %v, want solo", got)
	}
}

func TestFlattensGroupsToDottedKeys(t *testing.T) {
	f, cap := newForwarder(t, func(c *Config) { c.BatchSize = 1 })
	log := slog.New(f.Handler())

	log.WithGroup("http").Info("req", "status", 500, "method", "GET")

	waitFor(t, time.Second, func() bool { return cap.count() >= 1 })
	line := cap.snapshot()[0]
	if got := line["http.status"]; got != float64(500) {
		t.Errorf("http.status = %v (%T), want 500", got, got)
	}
	if got := line["http.method"]; got != "GET" {
		t.Errorf("http.method = %v, want GET", got)
	}
}

func TestWithAttrsCarriedThrough(t *testing.T) {
	f, cap := newForwarder(t, func(c *Config) { c.BatchSize = 1 })
	log := slog.New(f.Handler()).With("request_id", "abc123")

	log.Info("handling")

	waitFor(t, time.Second, func() bool { return cap.count() >= 1 })
	if got := cap.snapshot()[0]["request_id"]; got != "abc123" {
		t.Errorf("request_id = %v, want abc123", got)
	}
}

func TestReservedKeysAreNamespaced(t *testing.T) {
	f, cap := newForwarder(t, func(c *Config) { c.BatchSize = 1 })
	log := slog.New(f.Handler())

	// User attrs collide with canonical fields; ours must win, theirs preserved.
	log.Info("real message", "message", "user msg", "service", "user svc")

	waitFor(t, time.Second, func() bool { return cap.count() >= 1 })
	line := cap.snapshot()[0]
	if got := line["message"]; got != "real message" {
		t.Errorf("message = %v, want 'real message'", got)
	}
	if got := line["service"]; got != "omni-notify" {
		t.Errorf("service = %v, want omni-notify", got)
	}
	if got := line["attr.message"]; got != "user msg" {
		t.Errorf("attr.message = %v, want 'user msg'", got)
	}
	if got := line["attr.service"]; got != "user svc" {
		t.Errorf("attr.service = %v, want 'user svc'", got)
	}
}

func TestNonBlockingDropWhenBufferFull(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cap.record(t, r.Header, body)
		once.Do(func() { close(started) })
		<-release // block the shipper inside post()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	f, err := New(Config{
		Endpoint:      srv.URL,
		APIKey:        "k",
		Service:       "omni-notify",
		BatchSize:     1, // first record posts immediately, blocking the shipper
		BufferSize:    1, // only one record can queue while blocked
		FlushInterval: time.Hour,
		Timeout:       time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(f.Handler())

	log.Info("trigger") // shipper takes this, posts, blocks on release
	<-started

	// Shipper is now stuck in post(). Buffer holds 1; the rest must be dropped,
	// and every call must return immediately (never block).
	for i := 0; i < 20; i++ {
		log.Info("overflow")
	}
	if f.Dropped() == 0 {
		t.Fatalf("expected dropped records when buffer full, got 0")
	}

	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = f.Shutdown(ctx)
}

func TestShutdownDrainsBufferedRecords(t *testing.T) {
	f, cap := newForwarder(t, func(c *Config) {
		c.BatchSize = 100           // no size flush
		c.FlushInterval = time.Hour // no interval flush
	})
	log := slog.New(f.Handler())

	log.Info("a")
	log.Info("b")
	log.Info("c")

	// Nothing should have flushed yet via size/interval; Shutdown must drain.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if cap.count() != 3 {
		t.Errorf("after drain got %d records, want 3", cap.count())
	}
}

func TestMultiHandlerFansOut(t *testing.T) {
	f, cap := newForwarder(t, func(c *Config) { c.BatchSize = 1 })

	var stdout bytes.Buffer
	text := slog.NewTextHandler(&stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	log := slog.New(NewMultiHandler(text, f.Handler()))

	log.Info("hello", "k", "v")

	waitFor(t, time.Second, func() bool { return cap.count() >= 1 })

	if s := stdout.String(); !bytes.Contains([]byte(s), []byte("hello")) || !bytes.Contains([]byte(s), []byte("k=v")) {
		t.Errorf("stdout handler missing record: %q", s)
	}
	if got := cap.snapshot()[0]["message"]; got != "hello" {
		t.Errorf("forwarded message = %v, want hello", got)
	}
}

func TestNewRejectsMissingEndpointOrKey(t *testing.T) {
	if _, err := New(Config{APIKey: "k"}); err == nil {
		t.Error("expected error when endpoint missing")
	}
	if _, err := New(Config{Endpoint: "http://x"}); err == nil {
		t.Error("expected error when api_key missing")
	}
}
