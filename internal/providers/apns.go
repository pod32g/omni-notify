package providers

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
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
)

// apns sends Apple alert pushes to the devices an event names, over HTTP/2
// with a token-based (.p8) credential. Notify keeps no device list: the
// producer (the on-call plugin) knows whose phones to ring.
//
// secret: the .p8 key from Apple, PEM (required, unless key_from).
// config:
//   - team_id, key_id: the Apple Developer team and the key's id (required)
//   - topic: the app's bundle id (required)
//   - key_from: the name of another apns provider whose .p8 to use instead of
//     a secret of its own (same key_id and team_id). An Apple key serves every
//     app of its team, so a second app needs no copy of the key.
//   - development_url, production_url: endpoint overrides, mainly for tests
//
// The event's annotations say where and how:
//   - apns_tokens: comma-separated "<environment>:<token>" (environment
//     development or production; a bare token is production) — required
//   - apns_category, apns_thread_id, apns_interruption_level
//     (passive, active, time-sensitive, critical), apns_collapse_id
//   - apns_data: a JSON object added to the payload beside "aps"
//
// The alert is the producer's message title and text when it wrote one, else
// the event's title and summary. A token Apple says is gone is skipped and
// logged; the send fails only when no device got it.
type apns struct {
	client               *http.Client
	keyID, teamID, topic string
	key                  *ecdsa.PrivateKey
	devURL, prodURL      string

	mu      sync.Mutex
	token   string
	tokenAt time.Time
}

const (
	apnsProduction  = "https://api.push.apple.com"
	apnsDevelopment = "https://api.sandbox.push.apple.com"
	// APNs refuses provider tokens older than an hour and throttles
	// refreshes more often than every 20 minutes.
	apnsTokenLifetime = 40 * time.Minute
)

var apnsInterruptionLevels = map[string]bool{"passive": true, "active": true, "time-sensitive": true, "critical": true}

// apnsClient is one HTTP/2 client per process: Apple wants connections
// reused, and a provider is rebuilt for every delivery.
var apnsClient = &http.Client{
	Timeout:   10 * time.Second,
	Transport: &http.Transport{ForceAttemptHTTP2: true, TLSHandshakeTimeout: 5 * time.Second, IdleConnTimeout: 5 * time.Minute},
}

// apnsTokens caches provider tokens per key across rebuilt providers.
var apnsTokens sync.Map // key id + team → *apnsToken

type apnsToken struct {
	mu  sync.Mutex
	jwt string
	at  time.Time
}

func newAPNS(cfg map[string]any, secret string, allowPrivate bool) (Provider, error) {
	a := &apns{client: apnsClient, keyID: cfgString(cfg, "key_id"), teamID: cfgString(cfg, "team_id"),
		topic: cfgString(cfg, "topic"), devURL: cfgStringDefault(cfg, "development_url", apnsDevelopment),
		prodURL: cfgStringDefault(cfg, "production_url", apnsProduction)}
	switch {
	case a.keyID == "" || a.teamID == "":
		return nil, errors.New("apns: team_id and key_id are required")
	case a.topic == "":
		return nil, errors.New("apns: topic (the app's bundle id) is required")
	case secret == "":
		return nil, errors.New("apns: the .p8 key secret is required")
	}
	for _, u := range []string{a.devURL, a.prodURL} {
		if err := validateHTTPURL(u, allowPrivate); err != nil {
			return nil, fmt.Errorf("apns: %w", err)
		}
	}
	key, err := parseP8([]byte(secret))
	if err != nil {
		return nil, err
	}
	a.key = key
	return a, nil
}

// apnsKeyFrom returns the .p8 of the stored apns provider `from`, for a
// provider whose config says key_from. Only the key is shared: the topic stays
// the borrower's, and its key_id and team_id must name the same key.
func apnsKeyFrom(src ProviderSource, from string, cfg map[string]any, secret string) (string, error) {
	switch {
	case secret != "":
		return "", errors.New("apns: give the .p8 secret or key_from, not both")
	case src == nil:
		return "", errors.New("apns: key_from needs the provider store")
	}
	p, err := src.GetProvider(context.Background(), from)
	if err != nil {
		return "", fmt.Errorf("apns: key_from %q: %w", from, err)
	}
	keyID, teamID := cfgString(p.Config, "key_id"), cfgString(p.Config, "team_id")
	switch {
	case p.Kind != "apns":
		return "", fmt.Errorf("apns: key_from %q is a %s provider, not apns", from, p.Kind)
	case cfgString(p.Config, "key_from") != "":
		return "", fmt.Errorf("apns: key_from %q borrows its key too; name the provider that holds it", from)
	case keyID != cfgString(cfg, "key_id") || teamID != cfgString(cfg, "team_id"):
		return "", fmt.Errorf("apns: key_from %q holds key %s of team %s, not the key_id and team_id given", from, keyID, teamID)
	}
	return p.Secret, nil
}

// withClient replaces the HTTP client (tests).
func (a *apns) withClient(c *http.Client) *apns { a.client = c; return a }

func (a *apns) Send(ctx context.Context, msg models.NotificationMessage) error {
	ev := msg.Event
	targets := parseAPNSTokens(ev.Annotations["apns_tokens"])
	if len(targets) == 0 {
		return errors.New("apns: the event names no devices (annotation apns_tokens)")
	}
	body, err := apnsPayload(ev)
	if err != nil {
		return err
	}
	var sent int
	var failures []string
	for _, t := range targets {
		err := a.push(ctx, t, body, ev.Annotations["apns_collapse_id"])
		switch {
		case err == nil:
			sent++
		case errors.Is(err, errAPNSGone):
			slog.Warn("apns: skipping a device token Apple says is gone", "token", t.token[:min(8, len(t.token))], "err", err)
		default:
			failures = append(failures, err.Error())
		}
	}
	if sent > 0 {
		if len(failures) > 0 {
			slog.Warn("apns: some devices were not reached", "sent", sent, "failed", strings.Join(failures, "; "))
		}
		return nil
	}
	if len(failures) == 0 {
		return errors.New("apns: every device token is gone")
	}
	return errors.New(strings.Join(failures, "; "))
}

type apnsTarget struct{ env, token string }

func parseAPNSTokens(raw string) []apnsTarget {
	var out []apnsTarget
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		env, tok := "production", part
		if e, t, ok := strings.Cut(part, ":"); ok {
			env, tok = e, t
		}
		out = append(out, apnsTarget{env: env, token: tok})
	}
	return out
}

// apnsPayload is the alert and the producer's data. A lock screen shows it,
// so it carries no more than the event's title and summary.
func apnsPayload(ev models.Event) ([]byte, error) {
	title, text := ev.Title, ev.Summary
	if ev.Message != nil && ev.Message.Title != "" {
		title = ev.Message.Title
	}
	if ev.Message.Custom() {
		title, text = headline(ev), ev.Message.Text
	}
	if text == "" {
		text = ev.Description
	}
	aps := map[string]any{
		"alert": map[string]string{"title": truncate(title, 200), "body": truncate(text, 1000)},
		"sound": "default",
	}
	if c := ev.Annotations["apns_category"]; c != "" {
		aps["category"] = c
	}
	if t := ev.Annotations["apns_thread_id"]; t != "" {
		aps["thread-id"] = t
	}
	if l := ev.Annotations["apns_interruption_level"]; l != "" {
		if !apnsInterruptionLevels[l] {
			return nil, fmt.Errorf("apns: unknown interruption level %q", l)
		}
		aps["interruption-level"] = l
	}
	payload := map[string]any{}
	if raw := ev.Annotations["apns_data"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			return nil, fmt.Errorf("apns: apns_data is not a JSON object: %w", err)
		}
	}
	payload["aps"] = aps
	return json.Marshal(payload)
}

var errAPNSGone = errors.New("apns: device token is no longer valid")

// push sends to one device, retrying once with a fresh provider token when
// Apple says it expired or is invalid.
func (a *apns) push(ctx context.Context, t apnsTarget, body []byte, collapseID string) error {
	base := a.prodURL
	switch t.env {
	case "development":
		base = a.devURL
	case "production":
	default:
		return fmt.Errorf("apns: unknown environment %q", t.env)
	}
	for attempt := 0; attempt < 2; attempt++ {
		jwt, err := a.providerToken(attempt > 0)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/3/device/"+t.token, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "bearer "+jwt)
		req.Header.Set("apns-topic", a.topic)
		req.Header.Set("apns-push-type", "alert")
		req.Header.Set("apns-priority", "10")
		if collapseID != "" {
			req.Header.Set("apns-collapse-id", truncate(collapseID, 64))
		}
		resp, err := a.client.Do(req)
		if err != nil {
			return fmt.Errorf("apns: %w", err)
		}
		var r struct {
			Reason string `json:"reason"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		_ = json.Unmarshal(b, &r)
		switch {
		case resp.StatusCode == http.StatusOK:
			return nil
		case resp.StatusCode == http.StatusGone || r.Reason == "BadDeviceToken" || r.Reason == "Unregistered" || r.Reason == "DeviceTokenNotForTopic":
			return fmt.Errorf("%w (%d %s)", errAPNSGone, resp.StatusCode, r.Reason)
		case resp.StatusCode == http.StatusForbidden && (r.Reason == "ExpiredProviderToken" || r.Reason == "InvalidProviderToken") && attempt == 0:
			continue
		default:
			return fmt.Errorf("apns: %d %s", resp.StatusCode, r.Reason)
		}
	}
	return errors.New("apns: provider token refused")
}

// providerToken returns a cached ES256 JWT, minting one when stale or forced.
func (a *apns) providerToken(force bool) (string, error) {
	v, _ := apnsTokens.LoadOrStore(a.teamID+"/"+a.keyID, &apnsToken{})
	c := v.(*apnsToken)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if !force && c.jwt != "" && now.Sub(c.at) < apnsTokenLifetime {
		return c.jwt, nil
	}
	jwt, err := signES256(a.key, map[string]any{"alg": "ES256", "kid": a.keyID}, map[string]any{"iss": a.teamID, "iat": now.Unix()})
	if err != nil {
		return "", err
	}
	c.jwt, c.at = jwt, now
	return jwt, nil
}

// parseP8 reads Apple's .p8 key: PEM, or PEM whose line breaks became spaces
// (a YAML or environment value folds them), or the bare base64 body.
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

// signES256 builds a compact JWS with the raw r||s signature APNs expects.
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
