package config

import (
	"testing"
	"time"
)

func TestParse_EnvExpansionAndDefaults(t *testing.T) {
	t.Setenv("TEST_TOKEN", "secret-token")
	t.Setenv("TEST_HOOK", "https://example.com/hook")

	raw := []byte(`
security:
  tokens:
    - "${TEST_TOKEN}"
providers:
  - name: d
    kind: discord
    secret: "${TEST_HOOK}"
routes:
  - name: all
    is_default: true
    providers: [d]
    dedup_window: 10m
`)
	cfg, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Security.Tokens[0] != "secret-token" {
		t.Errorf("token not expanded: %q", cfg.Security.Tokens[0])
	}
	if cfg.Providers[0].Secret != "https://example.com/hook" {
		t.Errorf("secret not expanded: %q", cfg.Providers[0].Secret)
	}
	// defaults applied
	if cfg.Server.Addr != ":8080" {
		t.Errorf("default addr not applied: %q", cfg.Server.Addr)
	}
	if cfg.Delivery.Workers != 4 {
		t.Errorf("default workers not applied: %d", cfg.Delivery.Workers)
	}
	if cfg.Dedupe.DefaultWindow.D() != 5*time.Minute {
		t.Errorf("default dedup window not applied: %v", cfg.Dedupe.DefaultWindow.D())
	}
	if cfg.Routes[0].DedupWindow.D() != 10*time.Minute {
		t.Errorf("route dedup window: %v", cfg.Routes[0].DedupWindow.D())
	}
}

func TestParse_RejectsMissingToken(t *testing.T) {
	if _, err := Parse([]byte(`storage: {path: x.db}`)); err == nil {
		t.Fatal("expected error for missing tokens")
	}
}

func TestParse_RejectsUnresolvedEnvToken(t *testing.T) {
	raw := []byte(`security: {tokens: ["${DEFINITELY_UNSET_VAR_XYZ}"]}`)
	if _, err := Parse(raw); err == nil {
		t.Fatal("expected error for unresolved env token")
	}
}

func TestParse_RejectsRouteWithUnknownProvider(t *testing.T) {
	t.Setenv("TT", "tok")
	raw := []byte(`
security: {tokens: ["${TT}"]}
routes:
  - name: r
    providers: [does-not-exist]
`)
	if _, err := Parse(raw); err == nil {
		t.Fatal("expected error for route referencing unknown provider")
	}
}

func TestParse_RejectsDuplicateProvider(t *testing.T) {
	t.Setenv("TT", "tok")
	raw := []byte(`
security: {tokens: ["${TT}"]}
providers:
  - {name: d, kind: discord}
  - {name: d, kind: slack}
`)
	if _, err := Parse(raw); err == nil {
		t.Fatal("expected error for duplicate provider name")
	}
}

func TestParse_ForwardDisabledByDefault(t *testing.T) {
	t.Setenv("TT", "tok")
	cfg, err := Parse([]byte(`security: {tokens: ["${TT}"]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Log.Forward.Enabled {
		t.Error("forward should be disabled when omitted")
	}
}

func TestParse_ForwardDefaultsWhenEnabled(t *testing.T) {
	t.Setenv("TT", "tok")
	t.Setenv("LOG_KEY", "ingest-key")
	raw := []byte(`
security: {tokens: ["${TT}"]}
log:
  forward:
    enabled: true
    endpoint: "http://192.168.68.34:8080/api/v1/ingest"
    api_key: "${LOG_KEY}"
`)
	cfg, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	fw := cfg.Log.Forward
	if !fw.Enabled {
		t.Fatal("forward should be enabled")
	}
	if fw.APIKey != "ingest-key" {
		t.Errorf("api_key not expanded: %q", fw.APIKey)
	}
	if fw.Service != "omni-notify" {
		t.Errorf("default service = %q, want omni-notify", fw.Service)
	}
	if fw.BatchSize != 100 {
		t.Errorf("default batch_size = %d, want 100", fw.BatchSize)
	}
	if fw.BufferSize != 10000 {
		t.Errorf("default buffer_size = %d, want 10000", fw.BufferSize)
	}
	if fw.FlushInterval.D() != 2*time.Second {
		t.Errorf("default flush_interval = %v, want 2s", fw.FlushInterval.D())
	}
	if fw.Timeout.D() != 5*time.Second {
		t.Errorf("default timeout = %v, want 5s", fw.Timeout.D())
	}
}

func TestParse_ForwardRequiresEndpointAndKey(t *testing.T) {
	t.Setenv("TT", "tok")
	missingEndpoint := []byte(`
security: {tokens: ["${TT}"]}
log: {forward: {enabled: true, api_key: k}}
`)
	if _, err := Parse(missingEndpoint); err == nil {
		t.Error("expected error when forward enabled without endpoint")
	}
	missingKey := []byte(`
security: {tokens: ["${TT}"]}
log: {forward: {enabled: true, endpoint: "http://x:8080/api/v1/ingest"}}
`)
	if _, err := Parse(missingKey); err == nil {
		t.Error("expected error when forward enabled without api_key")
	}
}
