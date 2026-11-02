package auth

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const issuer = "https://identity.omni.test"

type keys struct {
	rsa     *rsa.PrivateKey
	ed      ed25519.PrivateKey
	fetches atomic.Int32
	srv     *httptest.Server
}

func newKeys(t *testing.T) *keys {
	t.Helper()
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	_, ek, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k := &keys{rsa: rk, ed: ek}
	enc := base64.RawURLEncoding
	k.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k.fetches.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{
			{"kty": "RSA", "kid": "r1", "alg": "RS256", "use": "sig", "n": enc.EncodeToString(rk.N.Bytes()),
				"e": enc.EncodeToString(big.NewInt(int64(rk.E)).Bytes())},
			{"kty": "OKP", "crv": "Ed25519", "kid": "e1", "alg": "EdDSA", "use": "sig",
				"x": enc.EncodeToString(ek.Public().(ed25519.PublicKey))},
		}})
	}))
	t.Cleanup(k.srv.Close)
	return k
}

func (k *keys) sign(t *testing.T, alg, kid string, claims map[string]any) string {
	t.Helper()
	enc := base64.RawURLEncoding
	h, _ := json.Marshal(map[string]string{"alg": alg, "kid": kid, "typ": "JWT"})
	c, _ := json.Marshal(claims)
	signing := enc.EncodeToString(h) + "." + enc.EncodeToString(c)
	var sig []byte
	switch alg {
	case "RS256":
		sum := sha256.Sum256([]byte(signing))
		var err error
		sig, err = rsa.SignPKCS1v15(rand.Reader, k.rsa, crypto.SHA256, sum[:])
		if err != nil {
			t.Fatal(err)
		}
	case "EdDSA":
		sig = ed25519.Sign(k.ed, []byte(signing))
	default:
		sig = []byte("x")
	}
	return signing + "." + enc.EncodeToString(sig)
}

// machine returns Identity's client_credentials claims (sub = aud = client).
func machine(client string, now time.Time) map[string]any {
	return map[string]any{"iss": issuer, "sub": client, "aud": client, "iat": now.Unix(),
		"exp": now.Add(15 * time.Minute).Unix(), "scope": "notify", "token_use": "access"}
}

func TestVerifyClientCredentialsTokens(t *testing.T) {
	k := newKeys(t)
	now := time.Now()
	v := NewVerifier(issuer+"/", k.srv.URL, time.Minute, nil)
	for _, alg := range []struct{ alg, kid string }{{"RS256", "r1"}, {"EdDSA", "e1"}} {
		c, err := v.Verify(context.Background(), k.sign(t, alg.alg, alg.kid, machine("omni-notify-claude", now)))
		if err != nil {
			t.Fatalf("%s: %v", alg.alg, err)
		}
		if id, ok := c.MachineClient(); !ok || id != "omni-notify-claude" {
			t.Fatalf("%s: machine client %q %v", alg.alg, id, ok)
		}
	}
	if n := k.fetches.Load(); n != 1 {
		t.Fatalf("JWKS fetched %d times, want 1 (cached)", n)
	}
}

func TestVerifyRejects(t *testing.T) {
	k := newKeys(t)
	now := time.Now()
	v := NewVerifier(issuer, k.srv.URL, time.Minute, nil)
	with := func(mut func(map[string]any)) map[string]any {
		c := machine("omni-notify-claude", now)
		mut(c)
		return c
	}
	good := k.sign(t, "RS256", "r1", machine("omni-notify-claude", now))
	cases := map[string]string{
		"expired":        k.sign(t, "RS256", "r1", with(func(c map[string]any) { c["exp"] = now.Add(-2 * time.Minute).Unix() })),
		"wrong issuer":   k.sign(t, "RS256", "r1", with(func(c map[string]any) { c["iss"] = "https://evil" })),
		"id token":       k.sign(t, "RS256", "r1", with(func(c map[string]any) { c["token_use"] = "id" })),
		"future iat":     k.sign(t, "RS256", "r1", with(func(c map[string]any) { c["iat"] = now.Add(time.Hour).Unix() })),
		"alg none":       k.sign(t, "none", "r1", machine("omni-notify-claude", now)),
		"alg swap":       k.sign(t, "EdDSA", "r1", machine("omni-notify-claude", now)),
		"unknown kid":    k.sign(t, "RS256", "zz", machine("omni-notify-claude", now)),
		"tampered":       good[:len(good)-4] + "AAAA",
		"not a jwt":      "abc",
		"swapped claims": swapClaims(good, k.sign(t, "RS256", "r1", machine("omni-notify-gpt", now))),
	}
	for name, tok := range cases {
		if _, err := v.Verify(context.Background(), tok); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func swapClaims(a, b string) string {
	pa, pb := splitJWT(a), splitJWT(b)
	return pa[0] + "." + pb[1] + "." + pa[2]
}

func splitJWT(s string) [3]string {
	var out [3]string
	i := 0
	start := 0
	for j := 0; j < len(s); j++ {
		if s[j] == '.' {
			out[i] = s[start:j]
			i++
			start = j + 1
		}
	}
	out[2] = s[start:]
	return out
}

func TestUserTokensAreNotMachineClients(t *testing.T) {
	now := time.Now()
	c := Claims{Subject: "pod32g", Audience: Audience{"omni-notify-claude"}}
	if _, ok := c.MachineClient(); ok {
		t.Fatal("a user's token for the client must not act as the client")
	}
	c = Claims{Subject: "x", Audience: Audience{"x", "y"}}
	if _, ok := c.MachineClient(); ok {
		t.Fatal("multi-audience token accepted")
	}
	k := newKeys(t)
	v := NewVerifier(issuer, k.srv.URL, time.Minute, nil)
	claims := machine("omni-notify-claude", now)
	claims["sub"] = "pod32g"
	got, err := v.Verify(context.Background(), k.sign(t, "RS256", "r1", claims))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.MachineClient(); ok {
		t.Fatal("user token treated as machine token")
	}
}
