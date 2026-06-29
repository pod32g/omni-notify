package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/pod32g/omni-notify/internal/metrics"
	"github.com/pod32g/omni-notify/internal/notifier"
	"github.com/pod32g/omni-notify/internal/providers"
	"github.com/pod32g/omni-notify/internal/storage"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Config holds HTTP-server-level settings.
type Config struct {
	Addr               string
	Tokens             []string
	MaxBodyBytes       int64
	MetricsRequireAuth bool
	ReadTimeout        time.Duration
	WriteTimeout       time.Duration
}

// Server wires the REST API to the storage, notifier and metrics layers.
type Server struct {
	store    *storage.Store
	notifier *notifier.Notifier
	registry *providers.Registry
	metrics  *metrics.Metrics
	promReg  *prometheus.Registry
	log      *slog.Logger
	cfg      Config

	httpServer *http.Server
}

// NewServer constructs a Server. promReg is the registry exposed at /metrics.
func NewServer(
	store *storage.Store,
	n *notifier.Notifier,
	registry *providers.Registry,
	m *metrics.Metrics,
	promReg *prometheus.Registry,
	log *slog.Logger,
	cfg Config,
) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 1 << 20
	}
	s := &Server{
		store:    store,
		notifier: n,
		registry: registry,
		metrics:  m,
		promReg:  promReg,
		log:      log,
		cfg:      cfg,
	}
	// Build the HTTP server eagerly so the field is set before ListenAndServe is
	// launched in its own goroutine, avoiding a data race with Shutdown.
	s.httpServer = &http.Server{
		Addr:         cfg.Addr,
		Handler:      s.Handler(),
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	}
	return s
}

// Handler builds the fully wrapped HTTP handler (routes + middleware).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.promReg, promhttp.HandlerOpts{}))

	mux.HandleFunc("POST /api/v1/events", s.handleCreateEvent)
	mux.HandleFunc("GET /api/v1/events", s.handleListEvents)
	mux.HandleFunc("GET /api/v1/events/{id}", s.handleGetEvent)

	mux.HandleFunc("GET /api/v1/states", s.handleListStates)
	mux.HandleFunc("GET /api/v1/states/{fingerprint}", s.handleGetState)

	mux.HandleFunc("GET /api/v1/providers", s.handleListProviders)
	mux.HandleFunc("POST /api/v1/providers", s.handleCreateProvider)
	mux.HandleFunc("GET /api/v1/providers/{name}", s.handleGetProvider)
	mux.HandleFunc("PUT /api/v1/providers/{name}", s.handleReplaceProvider)
	mux.HandleFunc("PATCH /api/v1/providers/{name}", s.handlePatchProvider)

	mux.HandleFunc("GET /api/v1/routes", s.handleListRoutes)
	mux.HandleFunc("POST /api/v1/routes", s.handleCreateRoute)
	mux.HandleFunc("GET /api/v1/routes/{name}", s.handleGetRoute)
	mux.HandleFunc("PUT /api/v1/routes/{name}", s.handleReplaceRoute)
	mux.HandleFunc("PATCH /api/v1/routes/{name}", s.handlePatchRoute)

	mux.HandleFunc("GET /api/v1/deliveries", s.handleListDeliveries)
	mux.HandleFunc("POST /api/v1/test", s.handleTest)

	// Middleware chain, outermost first.
	var h http.Handler = mux
	h = s.authMiddleware(h)
	h = s.maxBodyMiddleware(h)
	h = s.logMiddleware(h)
	h = s.recoverMiddleware(h)
	return h
}

// ListenAndServe starts the HTTP server (blocking until Shutdown or error).
func (s *Server) ListenAndServe() error {
	s.log.Info("http server listening", "addr", s.cfg.Addr)
	err := s.httpServer.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}
