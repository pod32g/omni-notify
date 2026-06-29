// Package config loads and validates Omni-Notify's YAML configuration, resolving
// ${ENV} references so that secrets and tokens stay out of the file on disk.
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
	"gopkg.in/yaml.v3"
)

// Config is the full service configuration.
type Config struct {
	Server    ServerConfig   `yaml:"server"`
	Security  SecurityConfig `yaml:"security"`
	Storage   StorageConfig  `yaml:"storage"`
	Dedupe    DedupeConfig   `yaml:"dedupe"`
	Delivery  DeliveryConfig `yaml:"delivery"`
	Log       LogConfig      `yaml:"log"`
	Providers []ProviderSeed `yaml:"providers"`
	Routes    []RouteSeed    `yaml:"routes"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Addr         string          `yaml:"addr"`
	MaxBodyBytes int64           `yaml:"max_body_bytes"`
	ReadTimeout  models.Duration `yaml:"read_timeout"`
	WriteTimeout models.Duration `yaml:"write_timeout"`
}

// SecurityConfig holds authentication and encryption settings.
type SecurityConfig struct {
	// Tokens are accepted bearer tokens. Typically a single ${ENV} reference.
	Tokens []string `yaml:"tokens"`
	// EncryptionKey is a base64-encoded 32-byte key for secret encryption.
	EncryptionKey string `yaml:"encryption_key"`
	// MetricsRequireAuth gates /metrics behind bearer auth (default false).
	MetricsRequireAuth bool `yaml:"metrics_require_auth"`
	// AllowPrivateWebhookTargets, when true, permits provider URLs that resolve
	// to private/loopback/link-local/multicast addresses. Default false (secure).
	AllowPrivateWebhookTargets bool `yaml:"allow_private_webhook_targets"`
}

// StorageConfig holds persistence settings.
type StorageConfig struct {
	Path string `yaml:"path"`
}

// DedupeConfig holds default deduplication policy applied when a route does not
// specify its own.
type DedupeConfig struct {
	DefaultWindow         models.Duration `yaml:"default_window"`
	DefaultRepeatInterval models.Duration `yaml:"default_repeat_interval"`
}

// DeliveryConfig holds the worker pool and retry policy.
type DeliveryConfig struct {
	Workers       int             `yaml:"workers"`
	QueueSize     int             `yaml:"queue_size"`
	MaxAttempts   int             `yaml:"max_attempts"`
	BackoffBase   models.Duration `yaml:"backoff_base"`
	BackoffFactor float64         `yaml:"backoff_factor"`
	BackoffMax    models.Duration `yaml:"backoff_max"`
	SendTimeout   models.Duration `yaml:"send_timeout"`
	PollInterval  models.Duration `yaml:"poll_interval"`
}

// LogConfig controls slog output.
type LogConfig struct {
	Level   string        `yaml:"level"`   // debug|info|warn|error
	Format  string        `yaml:"format"`  // text|json
	Forward ForwardConfig `yaml:"forward"` // optional: ship logs to omni-logging
}

// ForwardConfig controls forwarding logs to an omni-logging ingest endpoint.
// When Enabled, records are shipped to Endpoint in addition to stdout (a tee),
// asynchronously and best-effort so logging never blocks the service.
type ForwardConfig struct {
	Enabled       bool            `yaml:"enabled"`
	Endpoint      string          `yaml:"endpoint"`       // POST /api/v1/ingest URL
	APIKey        string          `yaml:"api_key"`        // X-Api-Key header (supports ${ENV})
	Service       string          `yaml:"service"`        // "service" field (default "omni-notify")
	BatchSize     int             `yaml:"batch_size"`     // records per POST (default 100)
	BufferSize    int             `yaml:"buffer_size"`    // max queued before dropping (default 10000)
	FlushInterval models.Duration `yaml:"flush_interval"` // partial-batch flush (default 2s)
	Timeout       models.Duration `yaml:"timeout"`        // per-request timeout (default 5s)
}

// ProviderSeed is a provider defined in config and seeded into storage on boot.
type ProviderSeed struct {
	Name    string         `yaml:"name"`
	Kind    string         `yaml:"kind"`
	Secret  string         `yaml:"secret"`
	Config  map[string]any `yaml:"config"`
	Enabled *bool          `yaml:"enabled"` // nil => true
}

// ToModel converts the seed to a stored provider config.
func (s ProviderSeed) ToModel() models.ProviderConfig {
	enabled := true
	if s.Enabled != nil {
		enabled = *s.Enabled
	}
	return models.ProviderConfig{
		Name:      s.Name,
		Kind:      s.Kind,
		Config:    s.Config,
		Secret:    s.Secret,
		Enabled:   enabled,
		ManagedBy: models.ManagedByConfig,
	}
}

// RouteSeed is a route defined in config and seeded into storage on boot.
type RouteSeed struct {
	Name           string            `yaml:"name"`
	Match          map[string]string `yaml:"match"`
	Providers      []string          `yaml:"providers"`
	IsDefault      bool              `yaml:"is_default"`
	Disabled       bool              `yaml:"disabled"`
	Priority       int               `yaml:"priority"`
	StopProcessing bool              `yaml:"stop_processing"`
	DedupWindow    models.Duration   `yaml:"dedup_window"`
	RepeatInterval models.Duration   `yaml:"repeat_interval"`
}

// ToModel converts the seed to a stored route.
func (s RouteSeed) ToModel() models.Route {
	return models.Route{
		Name:           s.Name,
		Match:          s.Match,
		Providers:      s.Providers,
		IsDefault:      s.IsDefault,
		Disabled:       s.Disabled,
		Priority:       s.Priority,
		StopProcessing: s.StopProcessing,
		DedupWindow:    s.DedupWindow,
		RepeatInterval: s.RepeatInterval,
		ManagedBy:      models.ManagedByConfig,
	}
}

// Default returns a Config populated with sensible defaults.
func Default() Config {
	return Config{
		Server: ServerConfig{
			Addr:         ":8080",
			MaxBodyBytes: 1 << 20, // 1 MiB
			ReadTimeout:  models.Duration(15 * time.Second),
			WriteTimeout: models.Duration(15 * time.Second),
		},
		Security: SecurityConfig{
			MetricsRequireAuth: false,
		},
		Storage: StorageConfig{Path: "./omni-notify.db"},
		Dedupe: DedupeConfig{
			DefaultWindow:         models.Duration(5 * time.Minute),
			DefaultRepeatInterval: 0,
		},
		Delivery: DeliveryConfig{
			Workers:       4,
			QueueSize:     256,
			MaxAttempts:   5,
			BackoffBase:   models.Duration(2 * time.Second),
			BackoffFactor: 2.0,
			BackoffMax:    models.Duration(5 * time.Minute),
			SendTimeout:   models.Duration(10 * time.Second),
			PollInterval:  models.Duration(time.Second),
		},
		Log: LogConfig{Level: "info", Format: "text"},
	}
}

// Load reads, env-expands, parses, defaults, and validates a config file.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw)
}

// Parse env-expands and parses config from raw YAML bytes, applying defaults and
// validating the result. Exposed separately so tests need not touch disk.
func Parse(raw []byte) (Config, error) {
	expanded := os.Expand(string(raw), func(key string) string {
		return os.Getenv(key)
	})

	cfg := Default()
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyZeroDefaults()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// applyZeroDefaults restores defaults for fields that YAML left at the zero value
// because the key was present-but-empty or absent in a partial document.
func (c *Config) applyZeroDefaults() {
	d := Default()
	if c.Server.Addr == "" {
		c.Server.Addr = d.Server.Addr
	}
	if c.Server.MaxBodyBytes == 0 {
		c.Server.MaxBodyBytes = d.Server.MaxBodyBytes
	}
	if c.Server.ReadTimeout == 0 {
		c.Server.ReadTimeout = d.Server.ReadTimeout
	}
	if c.Server.WriteTimeout == 0 {
		c.Server.WriteTimeout = d.Server.WriteTimeout
	}
	if c.Storage.Path == "" {
		c.Storage.Path = d.Storage.Path
	}
	if c.Dedupe.DefaultWindow == 0 {
		c.Dedupe.DefaultWindow = d.Dedupe.DefaultWindow
	}
	if c.Delivery.Workers == 0 {
		c.Delivery.Workers = d.Delivery.Workers
	}
	if c.Delivery.QueueSize == 0 {
		c.Delivery.QueueSize = d.Delivery.QueueSize
	}
	if c.Delivery.MaxAttempts == 0 {
		c.Delivery.MaxAttempts = d.Delivery.MaxAttempts
	}
	if c.Delivery.BackoffBase == 0 {
		c.Delivery.BackoffBase = d.Delivery.BackoffBase
	}
	if c.Delivery.BackoffFactor == 0 {
		c.Delivery.BackoffFactor = d.Delivery.BackoffFactor
	}
	if c.Delivery.BackoffMax == 0 {
		c.Delivery.BackoffMax = d.Delivery.BackoffMax
	}
	if c.Delivery.SendTimeout == 0 {
		c.Delivery.SendTimeout = d.Delivery.SendTimeout
	}
	if c.Delivery.PollInterval == 0 {
		c.Delivery.PollInterval = d.Delivery.PollInterval
	}
	if c.Log.Level == "" {
		c.Log.Level = d.Log.Level
	}
	if c.Log.Format == "" {
		c.Log.Format = d.Log.Format
	}
	if c.Log.Forward.Enabled {
		fw := &c.Log.Forward
		if fw.Service == "" {
			fw.Service = "omni-notify"
		}
		if fw.BatchSize == 0 {
			fw.BatchSize = 100
		}
		if fw.BufferSize == 0 {
			fw.BufferSize = 10000
		}
		if fw.FlushInterval == 0 {
			fw.FlushInterval = models.Duration(2 * time.Second)
		}
		if fw.Timeout == 0 {
			fw.Timeout = models.Duration(5 * time.Second)
		}
	}
}

// Validate enforces invariants that defaults cannot supply.
func (c *Config) Validate() error {
	if len(c.Security.Tokens) == 0 {
		return fmt.Errorf("security.tokens must contain at least one bearer token")
	}
	for i, t := range c.Security.Tokens {
		if t == "" {
			return fmt.Errorf("security.tokens[%d] is empty (unresolved ${ENV}?)", i)
		}
	}
	if c.Delivery.BackoffFactor < 1 {
		return fmt.Errorf("delivery.backoff_factor must be >= 1")
	}
	if c.Delivery.MaxAttempts < 1 {
		return fmt.Errorf("delivery.max_attempts must be >= 1")
	}
	seenProviders := map[string]bool{}
	for _, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("provider name is required")
		}
		if seenProviders[p.Name] {
			return fmt.Errorf("duplicate provider name %q", p.Name)
		}
		seenProviders[p.Name] = true
		if p.Kind == "" {
			return fmt.Errorf("provider %q: kind is required", p.Name)
		}
	}
	seenRoutes := map[string]bool{}
	for _, r := range c.Routes {
		if r.Name == "" {
			return fmt.Errorf("route name is required")
		}
		if seenRoutes[r.Name] {
			return fmt.Errorf("duplicate route name %q", r.Name)
		}
		seenRoutes[r.Name] = true
		if len(r.Providers) == 0 {
			return fmt.Errorf("route %q: at least one provider is required", r.Name)
		}
		for _, pn := range r.Providers {
			if !seenProviders[pn] {
				return fmt.Errorf("route %q references unknown provider %q", r.Name, pn)
			}
		}
	}
	if c.Log.Forward.Enabled {
		fw := c.Log.Forward
		if fw.Endpoint == "" {
			return fmt.Errorf("log.forward.endpoint is required when forwarding is enabled")
		}
		if fw.APIKey == "" {
			return fmt.Errorf("log.forward.api_key is required when forwarding is enabled (unresolved ${ENV}?)")
		}
		if fw.BatchSize <= 0 {
			return fmt.Errorf("log.forward.batch_size must be > 0")
		}
		if fw.BufferSize <= 0 {
			return fmt.Errorf("log.forward.buffer_size must be > 0")
		}
		if fw.FlushInterval <= 0 {
			return fmt.Errorf("log.forward.flush_interval must be > 0")
		}
		if fw.Timeout <= 0 {
			return fmt.Errorf("log.forward.timeout must be > 0")
		}
	}
	return nil
}
