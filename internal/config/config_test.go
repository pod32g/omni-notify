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
