package oncall

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
	"slices"
	"strings"
	"sync"
	"time"
)

// Sign-in with Omni Identity: an access token signed by the issuer, for one of
// the configured audiences, names the person by username. The Omni On-Call app
// sends the token the phone's Omni Access sign-in mints for the Access
// gateway, so the gateway's client id is the usual audience.

// identityLeeway tolerates small clock differences between Identity and this host.
const identityLeeway = time.Minute

// keysTTL is how long fetched signing keys are trusted before a refetch, and
// refetchFloor stops an unknown kid from turning into a fetch per request.
const (
	keysTTL      = 10 * time.Minute
	refetchFloor = 30 * time.Second
)

// errIdentityDown means the signing keys could not be fetched, as opposed to a
// token that is wrong.
var errIdentityDown = errors.New("Omni Identity is unreachable")

type identityClaims struct {
	Issuer   string   `json:"iss"`
	Subject  string   `json:"sub"`
	Audience audience `json:"aud"`
	Expires  int64    `json:"exp"`
	Issued   int64    `json:"iat"`
	NotEarly int64    `json:"nbf"`
	TokenUse string   `json:"token_use"`
	Username string   `json:"preferred_username"`
	DeviceID string   `json:"device_id"`
}

// audience accepts a single string or an array.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// verifier checks Identity access tokens (EdDSA or RS256) with cached keys.
type verifier struct {
	issuer    string
	audiences []string
	client    *http.Client
	now       func() time.Time

	mu        sync.Mutex
	jwksURL   string // from config, or discovered on first use
	keys      map[string]crypto.PublicKey
	fetchedAt time.Time
}

func newVerifier(c IdentityConfig) *verifier {
	return &verifier{
		issuer:    strings.TrimRight(c.Issuer, "/"),
		audiences: c.Audiences,
		jwksURL:   c.JWKSURL,
		client:    &http.Client{Timeout: 5 * time.Second},
		now:       time.Now,
	}
}

// looksLikeJWT tells an Identity token from a personal token.
func looksLikeJWT(tok string) bool { return strings.Count(tok, ".") == 2 }

// verify checks the signature, issuer, audience, lifetime and token use.
func (v *verifier) verify(ctx context.Context, token string) (identityClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return identityClaims{}, errors.New("not a JWT")
	}
	enc := base64.RawURLEncoding
	hb, err := enc.DecodeString(parts[0])
	if err != nil {
		return identityClaims{}, errors.New("bad header")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(hb, &hdr); err != nil {
		return identityClaims{}, errors.New("bad header")
	}
	sig, err := enc.DecodeString(parts[2])
	if err != nil {
		return identityClaims{}, errors.New("bad signature encoding")
	}
	key, err := v.key(ctx, hdr.Kid)
	if err != nil {
		return identityClaims{}, err
	}
	signed := []byte(parts[0] + "." + parts[1])
	switch k := key.(type) {
	case *rsa.PublicKey:
		if hdr.Alg != "RS256" {
			return identityClaims{}, fmt.Errorf("alg %q does not match an RSA key", hdr.Alg)
		}
		sum := sha256.Sum256(signed)
		if err := rsa.VerifyPKCS1v15(k, crypto.SHA256, sum[:], sig); err != nil {
			return identityClaims{}, errors.New("bad signature")
		}
	case ed25519.PublicKey:
		if hdr.Alg != "EdDSA" {
			return identityClaims{}, fmt.Errorf("alg %q does not match an Ed25519 key", hdr.Alg)
		}
		if !ed25519.Verify(k, signed, sig) {
			return identityClaims{}, errors.New("bad signature")
		}
	default:
		return identityClaims{}, errors.New("unsupported key type")
	}
	cb, err := enc.DecodeString(parts[1])
	if err != nil {
		return identityClaims{}, errors.New("bad claims")
	}
	var c identityClaims
	if err := json.Unmarshal(cb, &c); err != nil {
		return identityClaims{}, errors.New("bad claims")
	}
	now := v.now()
	switch {
	case strings.TrimRight(c.Issuer, "/") != v.issuer:
		return identityClaims{}, errors.New("wrong issuer")
	case !slices.ContainsFunc(c.Audience, func(a string) bool { return slices.Contains(v.audiences, a) }):
		return identityClaims{}, errors.New("not issued for this server")
	case c.Expires == 0 || now.After(time.Unix(c.Expires, 0).Add(identityLeeway)):
		return identityClaims{}, errors.New("token expired")
	case c.NotEarly != 0 && now.Add(identityLeeway).Before(time.Unix(c.NotEarly, 0)):
		return identityClaims{}, errors.New("token not yet valid")
	case c.Issued != 0 && now.Add(identityLeeway).Before(time.Unix(c.Issued, 0)):
		return identityClaims{}, errors.New("token issued in the future")
	case c.TokenUse != "" && c.TokenUse != "access":
		return identityClaims{}, errors.New("not an access token")
	case c.Subject == "":
		return identityClaims{}, errors.New("token names no one")
	}
	return c, nil
}

func (v *verifier) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	age := v.now().Sub(v.fetchedAt)
	if k, ok := v.keys[kid]; ok && age < keysTTL {
		return k, nil
	}
	if v.keys == nil || age >= refetchFloor {
		keys, err := v.fetch(ctx)
		if err != nil {
			if k, ok := v.keys[kid]; ok {
				return k, nil // the cached key while Identity is unreachable
			}
			return nil, fmt.Errorf("%w: %v", errIdentityDown, err)
		}
		v.keys, v.fetchedAt = keys, v.now()
	}
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("unknown signing key")
}

// fetch reads the key set, discovering where it is the first time. Called
// with v.mu held.
func (v *verifier) fetch(ctx context.Context) (map[string]crypto.PublicKey, error) {
	if v.jwksURL == "" {
		var doc struct {
			JWKSURI string `json:"jwks_uri"`
		}
		if err := v.getJSON(ctx, v.issuer+"/.well-known/openid-configuration", &doc); err != nil {
			return nil, fmt.Errorf("discovery: %w", err)
		}
		if doc.JWKSURI == "" {
			return nil, errors.New("discovery: no jwks_uri")
		}
		v.jwksURL = doc.JWKSURI
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
	if err := v.getJSON(ctx, v.jwksURL, &set); err != nil {
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

func (v *verifier) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}
