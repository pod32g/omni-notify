// Package auth verifies Omni-Identity access tokens against the issuer's JWKS.
package auth

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// leeway tolerates small clock differences between Identity and this host.
const leeway = time.Minute

// refetchFloor stops an unknown kid from turning into a JWKS fetch per request.
const refetchFloor = 30 * time.Second

// Claims are the access-token claims Omni-Notify uses.
type Claims struct {
	Issuer   string   `json:"iss"`
	Subject  string   `json:"sub"`
	Audience Audience `json:"aud"`
	Expires  int64    `json:"exp"`
	Issued   int64    `json:"iat"`
	NotEarly int64    `json:"nbf"`
	Scope    string   `json:"scope"`
	TokenUse string   `json:"token_use"`
}

// Audience accepts a single string or an array.
type Audience []string

// UnmarshalJSON implements json.Unmarshaler.
func (a *Audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = Audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// MachineClient returns the client id of a client_credentials token. Identity
// issues those with sub = aud = client id; a user's token for the same client
// has the user as sub and is not accepted as the client acting on its own.
func (c Claims) MachineClient() (string, bool) {
	if c.Subject == "" || len(c.Audience) != 1 || c.Audience[0] != c.Subject {
		return "", false
	}
	return c.Subject, true
}

// Verifier checks Identity JWTs (RS256 or EdDSA) with a cached JWKS.
type Verifier struct {
	issuer  string
	jwksURL string
	ttl     time.Duration
	client  *http.Client
	now     func() time.Time

	mu        sync.Mutex
	keys      map[string]crypto.PublicKey
	fetchedAt time.Time
}

// NewVerifier builds a verifier. client may be nil.
func NewVerifier(issuer, jwksURL string, ttl time.Duration, client *http.Client) *Verifier {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &Verifier{issuer: strings.TrimRight(issuer, "/"), jwksURL: jwksURL, ttl: ttl, client: client, now: time.Now}
}

// SetClock replaces the time source (tests).
func (v *Verifier) SetClock(now func() time.Time) { v.now = now }

// LooksLikeJWT reports whether a bearer value has the shape of a JWS.
func LooksLikeJWT(tok string) bool { return strings.Count(tok, ".") == 2 }

// Verify checks the signature, issuer, lifetime and token use.
func (v *Verifier) Verify(ctx context.Context, token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("not a JWT")
	}
	enc := base64.RawURLEncoding
	hb, err := enc.DecodeString(parts[0])
	if err != nil {
		return Claims{}, errors.New("bad header")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(hb, &hdr); err != nil {
		return Claims{}, errors.New("bad header")
	}
	sig, err := enc.DecodeString(parts[2])
	if err != nil {
		return Claims{}, errors.New("bad signature encoding")
	}
	key, err := v.key(ctx, hdr.Kid)
	if err != nil {
		return Claims{}, err
	}
	signed := []byte(parts[0] + "." + parts[1])
	switch k := key.(type) {
	case *rsa.PublicKey:
		if hdr.Alg != "RS256" {
			return Claims{}, fmt.Errorf("alg %q does not match an RSA key", hdr.Alg)
		}
		sum := sha256.Sum256(signed)
		if err := rsa.VerifyPKCS1v15(k, crypto.SHA256, sum[:], sig); err != nil {
			return Claims{}, errors.New("bad signature")
		}
	case ed25519.PublicKey:
		if hdr.Alg != "EdDSA" {
			return Claims{}, fmt.Errorf("alg %q does not match an Ed25519 key", hdr.Alg)
		}
		if !ed25519.Verify(k, signed, sig) {
			return Claims{}, errors.New("bad signature")
		}
	default:
		return Claims{}, errors.New("unsupported key type")
	}
	cb, err := enc.DecodeString(parts[1])
	if err != nil {
		return Claims{}, errors.New("bad claims")
	}
	var c Claims
	if err := json.Unmarshal(cb, &c); err != nil {
		return Claims{}, errors.New("bad claims")
	}
	now := v.now()
	switch {
	case strings.TrimRight(c.Issuer, "/") != v.issuer:
		return Claims{}, errors.New("wrong issuer")
	case c.Expires == 0 || now.After(time.Unix(c.Expires, 0).Add(leeway)):
		return Claims{}, errors.New("token expired")
	case c.NotEarly != 0 && now.Add(leeway).Before(time.Unix(c.NotEarly, 0)):
		return Claims{}, errors.New("token not yet valid")
	case c.Issued != 0 && now.Add(leeway).Before(time.Unix(c.Issued, 0)):
		return Claims{}, errors.New("token issued in the future")
	case c.TokenUse != "" && c.TokenUse != "access":
		return Claims{}, errors.New("not an access token")
	}
	return c, nil
}

func (v *Verifier) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	age := v.now().Sub(v.fetchedAt)
	if k, ok := v.keys[kid]; ok && age < v.ttl {
		return k, nil
	}
	if v.keys == nil || age >= refetchFloor {
		keys, err := v.fetch(ctx)
		if err != nil {
			if k, ok := v.keys[kid]; ok {
				return k, nil // serve the cached key while Identity is unreachable
			}
			return nil, err
		}
		v.keys, v.fetchedAt = keys, v.now()
	}
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("unknown signing key")
}

func (v *Verifier) fetch(ctx context.Context) (map[string]crypto.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks: status %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Crv string `json:"crv"`
			N   string `json:"n"`
			E   string `json:"e"`
			X   string `json:"x"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	enc := base64.RawURLEncoding
	keys := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
		switch {
		case k.Kty == "RSA":
			n, err1 := enc.DecodeString(k.N)
			e, err2 := enc.DecodeString(k.E)
			if err1 != nil || err2 != nil || len(e) > 4 {
				continue
			}
			keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		case k.Kty == "OKP" && k.Crv == "Ed25519":
			x, err := enc.DecodeString(k.X)
			if err != nil || len(x) != ed25519.PublicKeySize {
				continue
			}
			keys[k.Kid] = ed25519.PublicKey(x)
		}
	}
	return keys, nil
}
