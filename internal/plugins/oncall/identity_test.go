package oncall

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// identityServer is a stand-in Omni Identity: discovery and one Ed25519 key.
type identityServer struct {
	srv     *httptest.Server
	key     ed25519.PrivateKey
	fetches atomic.Int32
}

func newIdentityServer(t *testing.T) *identityServer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	is := &identityServer{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": is.srv.URL, "jwks_uri": is.srv.URL + "/jwks.json"})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		is.fetches.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "OKP", "crv": "Ed25519", "kid": "k1", "alg": "EdDSA", "use": "sig",
			"x": base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)),
		}}})
	})
	is.srv = httptest.NewServer(mux)
	t.Cleanup(is.srv.Close)
	return is
}

// token signs claims with key (the server's own unless given).
func (is *identityServer) token(t *testing.T, claims map[string]any, key ed25519.PrivateKey) string {
	t.Helper()
	if key == nil {
		key = is.key
	}
	enc := base64.RawURLEncoding
	h, _ := json.Marshal(map[string]string{"alg": "EdDSA", "kid": "k1", "typ": "JWT"})
	c, _ := json.Marshal(claims)
	signed := enc.EncodeToString(h) + "." + enc.EncodeToString(c)
	return signed + "." + enc.EncodeToString(ed25519.Sign(key, []byte(signed)))
}

func identityConfig(dbPath, issuer string) string {
	cfg := strings.Replace(testConfig(dbPath), "{id: ana, name: Ana Abe,", "{id: ana, name: Ana Abe, identity: ana.abe,", 1)
	return cfg + "identity: {issuer: " + issuer + ", audiences: [omni_gw]}\n"
}

func TestSignInWithOmniIdentity(t *testing.T) {
	is := newIdentityServer(t)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	hs := newHarnessFrom(t, identityConfig(filepath.Join(t.TempDir(), "oncall.db"), is.srv.URL), at)
	claims := func(edit func(map[string]any)) map[string]any {
		c := map[string]any{"iss": is.srv.URL, "sub": "0c1d-ana", "aud": []string{"omni_gw"}, "preferred_username": "ana.abe",
			"iat": at.Unix(), "exp": at.Add(10 * time.Minute).Unix(), "token_use": "access", "device_id": "dev1"}
		if edit != nil {
			edit(c)
		}
		return c
	}

	status, body := hs.call("GET", "/api/v1/me", is.token(t, claims(nil), nil), "", nil)
	if status != 200 || body["user"].(map[string]any)["id"] != "ana" {
		t.Fatalf("ana's Omni token: %d %v", status, body)
	}
	if status, _ := hs.call("GET", "/api/v1/incidents", is.token(t, claims(nil), nil), "", nil); status != 200 {
		t.Fatalf("second call: %d", status)
	}
	if n := is.fetches.Load(); n != 1 {
		t.Errorf("keys fetched %d times, want once", n)
	}

	// A subject works when the token carries no username.
	hs.p.dir.byIdentity["0c1d-ana"] = hs.p.dir.users["ana"]
	if status, body := hs.call("GET", "/api/v1/me", is.token(t, claims(func(c map[string]any) { delete(c, "preferred_username") }), nil), "", nil); status != 200 {
		t.Errorf("by subject: %d %v", status, body)
	}

	status, body = hs.call("GET", "/api/v1/me", is.token(t, claims(func(c map[string]any) { c["preferred_username"], c["sub"] = "zed", "0c1d-zed" }), nil), "", nil)
	if status != http.StatusForbidden || body["code"] != "not_in_directory" || !strings.Contains(body["error"].(string), "zed") {
		t.Errorf("someone not in the directory: %d %v", status, body)
	}

	_, other, _ := ed25519.GenerateKey(rand.Reader)
	for name, tok := range map[string]string{
		"wrong audience": is.token(t, claims(func(c map[string]any) { c["aud"] = "omni_other" }), nil),
		"wrong issuer":   is.token(t, claims(func(c map[string]any) { c["iss"] = "https://elsewhere" }), nil),
		"expired":        is.token(t, claims(func(c map[string]any) { c["exp"] = at.Add(-2 * time.Minute).Unix() }), nil),
		"id token":       is.token(t, claims(func(c map[string]any) { c["token_use"] = "id" }), nil),
		"forged":         is.token(t, claims(nil), other),
	} {
		if status, body := hs.call("GET", "/api/v1/me", tok, "", nil); status != http.StatusUnauthorized || body["code"] != "unauthorized" {
			t.Errorf("%s: %d %v", name, status, body)
		}
	}

	// Personal tokens still work beside it.
	if status, body := hs.call("GET", "/api/v1/me", "ben-token", "", nil); status != 200 || body["user"].(map[string]any)["id"] != "ben" {
		t.Errorf("personal token: %d %v", status, body)
	}
}

func TestSignInWhileIdentityIsDown(t *testing.T) {
	is := newIdentityServer(t)
	issuer := is.srv.URL
	is.srv.Close()
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	hs := newHarnessFrom(t, identityConfig(filepath.Join(t.TempDir(), "oncall.db"), issuer), at)
	tok := is.token(t, map[string]any{"iss": issuer, "sub": "s", "aud": "omni_gw", "preferred_username": "ana.abe",
		"exp": at.Add(time.Minute).Unix()}, nil)
	if status, body := hs.call("GET", "/api/v1/me", tok, "", nil); status != http.StatusServiceUnavailable || body["code"] != "identity_unavailable" {
		t.Errorf("Identity down: %d %v", status, body)
	}
}

func TestConfigRejectsBrokenIdentity(t *testing.T) {
	for name, cfg := range map[string]string{
		"identity without issuer": `users: [{id: a, name: A, identity: a}]`,
		"no audiences": `identity: {issuer: "https://id.example"}
users: [{id: a, name: A, identity: a}]`,
		"duplicate identity": `identity: {issuer: "https://id.example", audiences: [gw]}
users: [{id: a, name: A, identity: x}, {id: b, name: B, identity: x}]`,
		"not a URL": `identity: {issuer: id.example, audiences: [gw]}`,
	} {
		var node yaml.Node
		if err := yaml.Unmarshal([]byte(cfg), &node); err != nil {
			t.Fatal(err)
		}
		if _, err := New(*node.Content[0]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
