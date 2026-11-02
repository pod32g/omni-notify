package voice

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pod32g/omni-notify/internal/config"
	"github.com/pod32g/omni-notify/internal/models"
)

// Invitation is what a VoIP push carries: enough for the phone to report the
// call to CallKit at once and nothing more. No message text and no
// credentials; the phone fetches the call with its own device credential.
type Invitation struct {
	CallID      string    `json:"call_id"`
	CallerName  string    `json:"caller_name"`
	CallerID    string    `json:"caller_id"`
	Test        bool      `json:"test"`
	Simulated   bool      `json:"simulated"`
	ExpiresAt   time.Time `json:"-"`
	RingSeconds int       `json:"ring_seconds"`
}

// Pusher rings a device.
type Pusher interface {
	Push(ctx context.Context, d models.Device, inv Invitation) error
}

// ErrBadDeviceToken means APNs rejected the device token for good (the app
// was removed or the token rotated): the token should be cleared.
var ErrBadDeviceToken = errors.New("apns: device token is no longer valid")

const (
	apnsProduction  = "https://api.push.apple.com"
	apnsDevelopment = "https://api.sandbox.push.apple.com"
	// APNs rejects provider tokens older than an hour and throttles refreshes
	// more often than every 20 minutes.
	apnsTokenLifetime = 40 * time.Minute
)

// APNS sends VoIP pushes over HTTP/2 with a token-based (.p8) credential.
type APNS struct {
	keyID, teamID, topic string
	key                  *ecdsa.PrivateKey
	client               *http.Client
	devURL, prodURL      string
	now                  func() time.Time

	mu      sync.Mutex
	token   string
	tokenAt time.Time
}

// NewAPNS loads the .p8 key and builds an HTTP/2 client. client may be nil.
func NewAPNS(cfg config.APNSConfig, client *http.Client) (*APNS, error) {
	pemBytes := []byte(cfg.Key)
	if cfg.KeyPath != "" {
		b, err := os.ReadFile(cfg.KeyPath)
		if err != nil {
			return nil, fmt.Errorf("apns key: %w", err)
		}
		pemBytes = b
	}
	key, err := parseP8(pemBytes)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{
			Timeout:   10 * time.Second,
			Transport: &http.Transport{ForceAttemptHTTP2: true, TLSHandshakeTimeout: 5 * time.Second, IdleConnTimeout: 5 * time.Minute},
		}
	}
	a := &APNS{keyID: cfg.KeyID, teamID: cfg.TeamID, topic: cfg.Topic, key: key, client: client,
		devURL: apnsDevelopment, prodURL: apnsProduction, now: time.Now}
	if cfg.DevelopmentURL != "" {
		a.devURL = cfg.DevelopmentURL
	}
	if cfg.ProductionURL != "" {
		a.prodURL = cfg.ProductionURL
	}
	return a, nil
}

// APNSFromProvider takes the credentials of a Notify provider of kind apns
// (the on-call app's push), so VoIP pushes use the same key without a second
// copy of it. Its topic is the bundle id; Push adds ".voip".
func APNSFromProvider(p models.ProviderConfig) (config.APNSConfig, error) {
	if p.Kind != "apns" {
		return config.APNSConfig{}, fmt.Errorf("provider %s is %s, not apns", p.Name, p.Kind)
	}
	str := func(k string) string {
		s, _ := p.Config[k].(string)
		return strings.TrimSpace(s)
	}
	c := config.APNSConfig{KeyID: str("key_id"), TeamID: str("team_id"), Topic: str("topic"), Key: p.Secret}
	if c.KeyID == "" || c.TeamID == "" || c.Topic == "" || c.Key == "" {
		return config.APNSConfig{}, fmt.Errorf("provider %s lacks its key_id, team_id, topic or key", p.Name)
	}
	return c, nil
}

// parseP8 reads Apple's .p8: PEM, or the same folded onto one line or bare,
// as the apns provider accepts it.
func parseP8(b []byte) (*ecdsa.PrivateKey, error) {
	var der []byte
	if block, _ := pem.Decode(b); block != nil {
		der = block.Bytes
	} else {
		body := strings.NewReplacer("-----BEGIN PRIVATE KEY-----", "", "-----END PRIVATE KEY-----", "").Replace(string(b))
		body = strings.Join(strings.Fields(body), "")
		d, err := base64.StdEncoding.DecodeString(body)
		if err != nil || body == "" {
			return nil, errors.New("apns key: not PEM (expected the .p8 file from Apple)")
		}
		der = d
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("apns key: %w", err)
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("apns key: not an EC P-256 key")
	}
	return ek, nil
}

// Push sends one VoIP push. It retries once with a fresh provider token when
// APNs says the token expired or is invalid.
func (a *APNS) Push(ctx context.Context, d models.Device, inv Invitation) error {
	if d.VoIPToken == "" {
		return ErrBadDeviceToken
	}
	body, err := json.Marshal(struct {
		APS any `json:"aps"`
		Invitation
		ExpiresAt int64 `json:"expires_at"`
	}{APS: struct{}{}, Invitation: inv, ExpiresAt: inv.ExpiresAt.Unix()})
	if err != nil {
		return err
	}
	base := a.devURL
	if d.APNSEnvironment == "production" {
		base = a.prodURL
	}
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := a.providerToken(attempt > 0)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/3/device/"+d.VoIPToken, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "bearer "+tok)
		req.Header.Set("apns-topic", a.topic+".voip")
		req.Header.Set("apns-push-type", "voip")
		req.Header.Set("apns-priority", "10")
		req.Header.Set("apns-id", uuidFrom(inv.CallID+d.ID))
		// Never deliver an invitation after the call stopped ringing.
		req.Header.Set("apns-expiration", strconv.FormatInt(inv.ExpiresAt.Unix(), 10))
		resp, err := a.client.Do(req)
		if err != nil {
			return fmt.Errorf("apns: %w", err)
		}
		reason := apnsReason(resp)
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK:
			return nil
		case resp.StatusCode == http.StatusGone || reason == "BadDeviceToken" || reason == "Unregistered" || reason == "DeviceTokenNotForTopic":
			return fmt.Errorf("%w (%d %s)", ErrBadDeviceToken, resp.StatusCode, reason)
		case resp.StatusCode == http.StatusForbidden && (reason == "ExpiredProviderToken" || reason == "InvalidProviderToken") && attempt == 0:
			continue
		default:
			return fmt.Errorf("apns: %d %s", resp.StatusCode, reason)
		}
	}
	return errors.New("apns: provider token rejected")
}

func apnsReason(resp *http.Response) string {
	var r struct {
		Reason string `json:"reason"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = json.Unmarshal(b, &r)
	return r.Reason
}

// providerToken returns a cached ES256 JWT, minting a new one when stale or forced.
func (a *APNS) providerToken(force bool) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if !force && a.token != "" && now.Sub(a.tokenAt) < apnsTokenLifetime {
		return a.token, nil
	}
	tok, err := signES256(a.key, map[string]any{"alg": "ES256", "kid": a.keyID},
		map[string]any{"iss": a.teamID, "iat": now.Unix()})
	if err != nil {
		return "", err
	}
	a.token, a.tokenAt = tok, now
	return tok, nil
}

// signES256 builds a compact JWS with a raw r||s signature, as APNs expects.
func signES256(key *ecdsa.PrivateKey, header, claims map[string]any) (string, error) {
	h, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	c, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(h) + "." + enc.EncodeToString(c)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + enc.EncodeToString(sig), nil
}

// verifyES256 checks a token made by signES256 (tests and diagnostics).
func verifyES256(pub *ecdsa.PublicKey, token string) bool {
	i := strings.LastIndexByte(token, '.')
	if i < 0 || strings.Count(token, ".") != 2 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(token[i+1:])
	if err != nil || len(sig) != 64 {
		return false
	}
	sum := sha256.Sum256([]byte(token[:i]))
	return ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
}

// uuidFrom derives a stable UUID-shaped id (apns-id) from s.
func uuidFrom(s string) string {
	b := sha256.Sum256([]byte(s))
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
