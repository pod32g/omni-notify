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
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pod32g/omni-notify/internal/api"
	"github.com/pod32g/omni-notify/internal/clock"
	"github.com/pod32g/omni-notify/internal/config"
	"github.com/pod32g/omni-notify/internal/metrics"
	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/notifier"
	"github.com/pod32g/omni-notify/internal/providers"
	"github.com/pod32g/omni-notify/internal/storage"
	"github.com/prometheus/client_golang/prometheus"
)

func main() {
	// Subcommand: generate an encryption key and exit.
	if len(os.Args) > 1 && os.Args[1] == "genkey" {
		if err := printGenKey(); err != nil {
			fmt.Fprintln(os.Stderr, "genkey:", err)
			os.Exit(1)
		}
		return
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
	log := newLogger(cfg.Log)
	slog.SetDefault(log)

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
	registry := providers.NewDefault(httpClient, allowPrivate)

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

	deliveryCtx, cancelDelivery := context.WithCancel(context.Background())
	if err := n.Start(deliveryCtx); err != nil {
		cancelDelivery()
		return err
	}

	// HTTP server.
	srv := api.NewServer(store, n, registry, m, promReg, log, api.Config{
		Addr:               cfg.Server.Addr,
		Tokens:             cfg.Security.Tokens,
		MaxBodyBytes:       cfg.Server.MaxBodyBytes,
		MetricsRequireAuth: cfg.Security.MetricsRequireAuth,
		ReadTimeout:        cfg.Server.ReadTimeout.D(),
		WriteTimeout:       cfg.Server.WriteTimeout.D(),
	})

	serverErr := make(chan error, 1)
	go func() { serverErr <- srv.ListenAndServe() }()

	// Wait for signal or server error.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		cancelDelivery()
		n.Stop()
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
	cancelDelivery()
	n.Stop()
	log.Info("shutdown complete")
	return nil
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

func printGenKey() error {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	fmt.Println(base64.StdEncoding.EncodeToString(key))
	return nil
}

func newLogger(cfg config.LogConfig) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(cfg.Level)}
	var h slog.Handler
	if cfg.Format == "json" {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
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
