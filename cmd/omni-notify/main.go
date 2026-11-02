// Command omni-notify is a generic event notification service: it receives
// events, deduplicates and routes them, and delivers notifications through
// pluggable providers with retry and delivery tracking.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pod32g/omni-notify/internal/api"
	"github.com/pod32g/omni-notify/internal/auth"
	"github.com/pod32g/omni-notify/internal/clock"
	"github.com/pod32g/omni-notify/internal/config"
	"github.com/pod32g/omni-notify/internal/logship"
	"github.com/pod32g/omni-notify/internal/metrics"
	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/notifier"
	"github.com/pod32g/omni-notify/internal/plugin"
	"github.com/pod32g/omni-notify/internal/providers"
	"github.com/pod32g/omni-notify/internal/storage"
	"github.com/pod32g/omni-notify/internal/voice"
	"github.com/prometheus/client_golang/prometheus"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// Subcommand: generate an encryption key and exit.
	if len(os.Args) > 1 && os.Args[1] == "genkey" {
		if err := printGenKey(); err != nil {
			fmt.Fprintln(os.Stderr, "genkey:", err)
			os.Exit(1)
		}
		return
	}

	// Subcommand: health probe. Lets the distroless container self-check without
	// a shell or curl (used by the Docker healthcheck and CI deploy readiness).
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck(os.Args[2:]))
	}

	// Agent-side subcommands: the MCP server for AI agents, and a one-shot
	// call (test tooling). Both talk to a running Omni-Notify over HTTP.
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		if err := runMCP(); err != nil {
			fmt.Fprintln(os.Stderr, "mcp:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "call" {
		os.Exit(runCall(os.Args[2:]))
	}

	configPath := flag.String("config", envOr("OMNI_NOTIFY_CONFIG", "config.yaml"), "path to config file")
	flag.Parse()

	if err := run(*configPath); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	log, logShutdown, err := newLogger(cfg.Log)
	if err != nil {
		return fmt.Errorf("init logging: %w", err)
	}
	slog.SetDefault(log)
	if logShutdown != nil {
		// Drain buffered logs to omni-logging on every exit path.
		defer func() {
			drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = logShutdown(drainCtx)
		}()
	}
	log.Info("starting omni-notify", "version", version)

	// Resolve encryption key (config value wins; falls back to env).
	keyB64 := cfg.Security.EncryptionKey
	if keyB64 == "" {
		keyB64 = os.Getenv("OMNI_NOTIFY_ENCRYPTION_KEY")
	}
	cipher, err := storage.NewCipherFromBase64(keyB64)
	if err != nil {
		return fmt.Errorf("encryption key: %w", err)
	}

	clk := clock.Real{}
	store, err := storage.Open(cfg.Storage.Path, cipher, clk)
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()

	// Fail fast: encrypted secrets exist but no key configured.
	if cipher == nil {
		hasSecrets, err := store.HasEncryptedSecrets(ctx)
		if err != nil {
			return err
		}
		hasConfigSecret := false
		for _, p := range cfg.Providers {
			if p.Secret != "" {
				hasConfigSecret = true
				break
			}
		}
		if hasSecrets || hasConfigSecret {
			return fmt.Errorf("provider secrets are present but no encryption key is set; " +
				"set OMNI_NOTIFY_ENCRYPTION_KEY (generate one with `omni-notify genkey`)")
		}
	} else {
		// A key is configured: verify it can decrypt existing secrets so we fail
		// fast on a wrong (but well-formed) key instead of at first delivery.
		// Runs before seeding, which would otherwise re-encrypt config secrets
		// with the wrong key and mask the problem.
		if _, err := store.ListProviders(ctx); err != nil {
			return fmt.Errorf("cannot decrypt stored provider secrets "+
				"(wrong OMNI_NOTIFY_ENCRYPTION_KEY?): %w", err)
		}
	}

	// Seed config-managed providers and routes (hybrid model: SQLite is source of
	// truth; config re-syncs its own entities on boot).
	if err := store.SeedProviders(ctx, toProviderModels(cfg.Providers)); err != nil {
		return err
	}
	if err := store.SeedRoutes(ctx, toRouteModels(cfg.Routes)); err != nil {
		return err
	}
	log.Info("seeded config entities", "providers", len(cfg.Providers), "routes", len(cfg.Routes))

	// Metrics.
	promReg := prometheus.NewRegistry()
	m := metrics.New()
	m.MustRegister(promReg)

	// Providers + notifier. The HTTP client is guarded against SSRF to private
	// targets unless explicitly allowed in config.
	allowPrivate := cfg.Security.AllowPrivateWebhookTargets
	httpClient := providers.NewGuardedClient(cfg.Delivery.SendTimeout.D(), allowPrivate)
	registry := providers.NewDefault(httpClient, allowPrivate, providers.WithMessageStore(store))

	n := notifier.New(store, registry, m, clk, log, notifier.Config{
		Workers:               cfg.Delivery.Workers,
		QueueSize:             cfg.Delivery.QueueSize,
		MaxAttempts:           cfg.Delivery.MaxAttempts,
		BackoffBase:           cfg.Delivery.BackoffBase.D(),
		BackoffFactor:         cfg.Delivery.BackoffFactor,
		BackoffMax:            cfg.Delivery.BackoffMax.D(),
		SendTimeout:           cfg.Delivery.SendTimeout.D(),
		PollInterval:          cfg.Delivery.PollInterval.D(),
		DefaultDedupWindow:    cfg.Dedupe.DefaultWindow.D(),
		DefaultRepeatInterval: cfg.Dedupe.DefaultRepeatInterval.D(),
	})

	// VOICE_CALL channel (optional).
	var (
		voiceSvc *voice.Service
		verifier *auth.Verifier
	)
	if cfg.Voice.Enabled {
		voiceSvc, verifier, err = newVoice(ctx, cfg, store, m, n, registry, log)
		if err != nil {
			return err
		}
	}
	// Plugins start before deliveries, so a queued delivery to a plugin's
	// provider kind finds it registered.
	pluginHandlers, stopPlugins, err := startPlugins(ctx, cfg.Plugins, plugin.Host{
		Log:          log,
		Clock:        clk,
		Providers:    registry,
		Deliver:      n.Deliver,
		IsAdminToken: plugin.TokenMatcher(cfg.Security.Tokens),
	})
	if err != nil {
		return err
	}
	defer stopPlugins()

	deliveryCtx, cancelDelivery := context.WithCancel(context.Background())
	if err := n.Start(deliveryCtx); err != nil {
		cancelDelivery()
		return err
	}
	if voiceSvc != nil {
		voiceSvc.Start(deliveryCtx)
	}
	stopEngines := func() {
		cancelDelivery()
		n.Stop()
		if voiceSvc != nil {
			voiceSvc.Stop()
		}
	}

	// Telegram callback buttons: polls the bots of Telegram providers that have
	// callback_hosts (none by default) and runs the buttons pressed.
	callbacks := providers.NewTelegramCallbacks(store, allowPrivate, log)
	callbacksDone := make(chan struct{})
	go func() {
		defer close(callbacksDone)
		callbacks.Run(deliveryCtx)
	}()

	// HTTP server.
	srv := api.NewServer(store, n, registry, m, promReg, log, api.Config{
		Addr:               cfg.Server.Addr,
		Tokens:             cfg.Security.Tokens,
		MaxBodyBytes:       cfg.Server.MaxBodyBytes,
		MetricsRequireAuth: cfg.Security.MetricsRequireAuth,
		ReadTimeout:        cfg.Server.ReadTimeout.D(),
		WriteTimeout:       cfg.Server.WriteTimeout.D(),
		Voice:              voiceSvc,
		Identity:           verifier,
		Plugins:            pluginHandlers,
	})

	serverErr := make(chan error, 2)
	go func() { serverErr <- srv.ListenAndServe() }()

	// The phone door: only a call in progress, only device credentials.
	var phoneSrv *http.Server
	if voiceSvc != nil && cfg.Voice.PhoneAddr != "" {
		phoneSrv = srv.PhoneServer(cfg.Voice.PhoneAddr)
		go func() {
			log.Info("phone door listening", "addr", cfg.Voice.PhoneAddr, "url", cfg.Voice.PhoneURL)
			if err := phoneSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				serverErr <- err
			}
		}()
	}

	// Wait for signal or server error.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		stopEngines()
		<-callbacksDone
		return err
	case s := <-sig:
		log.Info("shutting down", "signal", s.String())
	}

	// Graceful shutdown: stop HTTP first, then drain delivery workers.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	if phoneSrv != nil {
		if err := phoneSrv.Shutdown(shutdownCtx); err != nil {
			log.Warn("phone door shutdown", "err", err)
		}
	}
	stopEngines()
	<-callbacksDone
	log.Info("shutdown complete")
	return nil
}

// newVoice builds the VOICE_CALL service: the APNs pusher (or none, so phones
// poll), the Identity verifier for agent tokens, and the voice_call provider
// kind that lets routes deliver events by phone.
func newVoice(ctx context.Context, cfg config.Config, store *storage.Store, m *metrics.Metrics, n *notifier.Notifier,
	registry *providers.Registry, log *slog.Logger) (*voice.Service, *auth.Verifier, error) {
	vc := cfg.Voice
	apnsCfg := vc.APNS
	if apnsCfg.Provider != "" {
		// The key lives in an apns provider made through the admin API. If it is
		// missing, calls still work by polling rather than Notify failing to start.
		p, err := store.GetProvider(ctx, apnsCfg.Provider)
		if err == nil {
			apnsCfg, err = voice.APNSFromProvider(p)
		}
		if err != nil {
			log.Error("voice: no APNs from provider; calls wait for phones to poll", "provider", vc.APNS.Provider, "err", err)
			apnsCfg = config.APNSConfig{}
		}
	}
	var pusher voice.Pusher
	if apnsCfg.Configured() {
		a, err := voice.NewAPNS(apnsCfg, nil)
		switch {
		case err == nil:
			pusher = a
		case vc.APNS.Provider != "":
			log.Error("voice: the provider's APNs key is unusable; calls wait for phones to poll", "provider", vc.APNS.Provider, "err", err)
		default:
			return nil, nil, err
		}
	} else {
		log.Warn("voice: APNs is not configured; calls wait for phones to poll (development only)")
	}
	var verifier *auth.Verifier
	if vc.Identity.Configured() {
		verifier = auth.NewVerifier(vc.Identity.Issuer, vc.Identity.JWKSURL, vc.Identity.CacheTTL.D(), nil)
	}
	svc := voice.NewService(vc, store, m, voice.Options{
		Pusher: pusher, Fallback: n, Log: log, Poll: cfg.Delivery.PollInterval.D(),
	})
	registry.Register("voice_call", func(c map[string]any, _ string) (providers.Provider, error) {
		return voice.NewRouteProvider(svc, c)
	})
	log.Info("voice calls enabled", "agents", len(vc.Agents), "users", len(vc.Users),
		"push", pusher != nil, "identity", verifier != nil)
	return svc, verifier, nil
}

func toProviderModels(seeds []config.ProviderSeed) []models.ProviderConfig {
	out := make([]models.ProviderConfig, 0, len(seeds))
	for _, s := range seeds {
		out = append(out, s.ToModel())
	}
	return out
}

func toRouteModels(seeds []config.RouteSeed) []models.Route {
	out := make([]models.Route, 0, len(seeds))
	for _, s := range seeds {
		out = append(out, s.ToModel())
	}
	return out
}

// runHealthcheck performs a one-shot GET against url and returns a process exit
// code: 0 if the response is 2xx, 1 otherwise. It has no external dependencies
// so it works inside a distroless image.
func runHealthcheck(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	url := fs.String("url", "http://localhost:8080/healthz", "health endpoint URL")
	timeout := fs.Duration("timeout", 5*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "healthcheck: unhealthy status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

func printGenKey() error {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	fmt.Println(base64.StdEncoding.EncodeToString(key))
	return nil
}

// newLogger builds the application logger. It always logs to stdout; when log
// forwarding is enabled it tees each record to omni-logging as well, returning a
// shutdown hook that drains buffered records (nil when forwarding is disabled).
func newLogger(cfg config.LogConfig) (*slog.Logger, func(context.Context) error, error) {
	level := parseLevel(cfg.Level)
	opts := &slog.HandlerOptions{Level: level}
	var stdout slog.Handler
	if cfg.Format == "json" {
		stdout = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		stdout = slog.NewTextHandler(os.Stdout, opts)
	}

	if !cfg.Forward.Enabled {
		return slog.New(stdout), nil, nil
	}

	fw, err := logship.New(logship.Config{
		Endpoint:      cfg.Forward.Endpoint,
		APIKey:        cfg.Forward.APIKey,
		Service:       cfg.Forward.Service,
		BatchSize:     cfg.Forward.BatchSize,
		BufferSize:    cfg.Forward.BufferSize,
		FlushInterval: cfg.Forward.FlushInterval.D(),
		Timeout:       cfg.Forward.Timeout.D(),
		Level:         level,
	})
	if err != nil {
		return nil, nil, err
	}
	logger := slog.New(logship.NewMultiHandler(stdout, fw.Handler()))
	return logger, fw.Shutdown, nil
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
