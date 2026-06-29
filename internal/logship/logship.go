// Package logship forwards omni-notify's slog records to an omni-logging ingest
// endpoint (POST /api/v1/ingest) as NDJSON. Delivery is asynchronous and
// best-effort: records are buffered on a bounded channel and shipped by a
// background goroutine, so logging never blocks request handling and an
// omni-logging outage can never slow or crash the service. When the buffer is
// full, records are dropped (counted) rather than blocking the caller, and the
// shipper's own failures are written to a side writer (stderr) rather than back
// through slog, to avoid a feedback loop.
package logship

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultService       = "omni-notify"
	defaultBatchSize     = 100
	defaultBufferSize    = 10000
	defaultFlushInterval = 2 * time.Second
	defaultTimeout       = 5 * time.Second
	defaultMaxRetries    = 2
	defaultRetryBackoff  = 250 * time.Millisecond
)

// Config configures a Forwarder. Endpoint and APIKey are required; everything
// else falls back to sensible defaults.
type Config struct {
	Endpoint      string        // omni-logging ingest URL (POST /api/v1/ingest)
	APIKey        string        // sent as the X-Api-Key header
	Service       string        // value of the "service" field (default "omni-notify")
	BatchSize     int           // flush once this many records are buffered
	BufferSize    int           // max records queued before new ones are dropped
	FlushInterval time.Duration // flush a partial batch after this long
	Timeout       time.Duration // per-request HTTP timeout
	Level         slog.Leveler  // minimum level to forward (default Info)
	MaxRetries    int           // extra attempts per batch on failure (default 2)
	RetryBackoff  time.Duration // pause between attempts (default 250ms)
	Client        *http.Client  // HTTP client (default plain client w/ Timeout)
	ErrOut        io.Writer     // where shipper errors go (default os.Stderr)
}

// Forwarder buffers slog records and ships them to omni-logging in batches.
type Forwarder struct {
	cfg      Config
	ch       chan []byte
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	dropped  int64
}

// New validates cfg, applies defaults, and starts the background shipper.
func New(cfg Config) (*Forwarder, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("logship: endpoint is required")
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("logship: api_key is required")
	}
	if cfg.Service == "" {
		cfg.Service = defaultService
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultBatchSize
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = defaultBufferSize
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = defaultFlushInterval
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.Level == nil {
		cfg.Level = slog.LevelInfo
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = defaultMaxRetries
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = defaultRetryBackoff
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: cfg.Timeout}
	}
	if cfg.ErrOut == nil {
		cfg.ErrOut = os.Stderr
	}

	f := &Forwarder{
		cfg:  cfg,
		ch:   make(chan []byte, cfg.BufferSize),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go f.run()
	return f, nil
}

// Handler returns an slog.Handler that feeds this forwarder.
func (f *Forwarder) Handler() slog.Handler { return &handler{f: f} }

// Dropped reports how many records were dropped because the buffer was full.
func (f *Forwarder) Dropped() int64 { return atomic.LoadInt64(&f.dropped) }

// Shutdown signals the shipper to drain and flush remaining records, returning
// when it finishes or ctx expires.
func (f *Forwarder) Shutdown(ctx context.Context) error {
	f.stopOnce.Do(func() { close(f.stop) })
	select {
	case <-f.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run is the background shipper: it accumulates a batch and flushes it when the
// batch is full, the flush interval elapses, or shutdown is requested.
func (f *Forwarder) run() {
	defer close(f.done)
	ticker := time.NewTicker(f.cfg.FlushInterval)
	defer ticker.Stop()

	batch := make([][]byte, 0, f.cfg.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		f.post(batch)
		batch = batch[:0]
	}
	add := func(line []byte) {
		batch = append(batch, line)
		if len(batch) >= f.cfg.BatchSize {
			flush()
		}
	}

	for {
		select {
		case line := <-f.ch:
			add(line)
		case <-ticker.C:
			flush()
		case <-f.stop:
			for {
				select {
				case line := <-f.ch:
					add(line)
				default:
					flush()
					return
				}
			}
		}
	}
}

// post sends a batch as NDJSON, retrying a few times before giving up. A
// give-up is reported to ErrOut (never back through slog).
func (f *Forwarder) post(batch [][]byte) {
	body := append(bytes.Join(batch, []byte("\n")), '\n')

	var lastErr error
	for attempt := 0; attempt <= f.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(f.cfg.RetryBackoff)
		}
		if err := f.postOnce(body); err == nil {
			return
		} else {
			lastErr = err
		}
	}
	fmt.Fprintf(f.cfg.ErrOut, "logship: dropping %d records after %d attempts: %v\n",
		len(batch), f.cfg.MaxRetries+1, lastErr)
}

func (f *Forwarder) postOnce(body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), f.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("X-Api-Key", f.cfg.APIKey)

	resp, err := f.cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ingest returned status %d", resp.StatusCode)
	}
	return nil
}

// handler is the slog.Handler backed by a Forwarder. It carries the accumulated
// group prefix and pre-flattened attrs (from WithGroup/WithAttrs).
type handler struct {
	f     *Forwarder
	group string // dotted prefix for record-time attrs, e.g. "http."
	attrs []kv   // flattened attrs from WithAttrs
}

type kv struct {
	k string
	v any
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.f.cfg.Level.Level()
}

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return h
	}
	nh := &handler{f: h.f, group: h.group}
	nh.attrs = make([]kv, len(h.attrs), len(h.attrs)+len(as))
	copy(nh.attrs, h.attrs)
	for _, a := range as {
		appendAttr(&nh.attrs, h.group, a)
	}
	return nh
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &handler{f: h.f, group: h.group + name + ".", attrs: h.attrs}
}

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	local := make([]kv, len(h.attrs), len(h.attrs)+r.NumAttrs())
	copy(local, h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&local, h.group, a)
		return true
	})

	m := make(map[string]any, len(local)+4)
	for _, p := range local {
		key := p.k
		if isReserved(key) {
			key = "attr." + key // never clobber a canonical field
		}
		m[key] = p.v
	}
	m["message"] = r.Message
	m["level"] = strings.ToLower(r.Level.String())
	if !r.Time.IsZero() {
		m["timestamp"] = r.Time.Format(time.RFC3339Nano)
	}
	m["service"] = h.f.cfg.Service

	line, err := json.Marshal(m)
	if err != nil {
		fmt.Fprintf(h.f.cfg.ErrOut, "logship: marshal record: %v\n", err)
		return nil
	}

	// Non-blocking send: drop rather than block the caller if the buffer is full.
	select {
	case h.f.ch <- line:
	default:
		atomic.AddInt64(&h.f.dropped, 1)
	}
	return nil
}

// appendAttr flattens a slog.Attr (recursing into groups) into dst, prefixing
// keys with the current dotted group path.
func appendAttr(dst *[]kv, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		gs := a.Value.Group()
		if len(gs) == 0 {
			return
		}
		p := prefix
		if a.Key != "" { // a group with an empty key is inlined
			p = prefix + a.Key + "."
		}
		for _, ga := range gs {
			appendAttr(dst, p, ga)
		}
		return
	}
	if a.Key == "" {
		return
	}
	*dst = append(*dst, kv{k: prefix + a.Key, v: a.Value.Any()})
}

func isReserved(k string) bool {
	switch k {
	case "message", "level", "timestamp", "service":
		return true
	}
	return false
}
