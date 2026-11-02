package providers

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pod32g/omni-notify/internal/models"
)

// fakeAPNs is Apple's push endpoint over HTTP/2: it checks the provider
// token and answers per device token.
type fakeAPNs struct {
	t   *testing.T
	pub *ecdsa.PublicKey
	srv *httptest.Server

	mu        sync.Mutex
	got       []apnsRequest
	answers   map[string][]int // device token → statuses to answer in turn
	reasons   map[int]string
	seenProto string
}

type apnsRequest struct {
	token   string
	header  http.Header
	payload map[string]any
}

func newFakeAPNs(t *testing.T) (*fakeAPNs, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	p8 := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	f := &fakeAPNs{t: t, pub: &key.PublicKey, answers: map[string][]int{},
		reasons: map[int]string{410: "Unregistered", 403: "ExpiredProviderToken", 500: "InternalServerError"}}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.srv.EnableHTTP2 = true
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f, p8
}

func (f *fakeAPNs) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seenProto = r.Proto
	tok := strings.TrimPrefix(r.URL.Path, "/3/device/")
	jwt := strings.TrimPrefix(r.Header.Get("authorization"), "bearer ")
	if !verifyES256(f.pub, jwt) {
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `{"reason":"InvalidProviderToken"}`)
		return
	}
	var payload map[string]any
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &payload)
	f.got = append(f.got, apnsRequest{token: tok, header: r.Header.Clone(), payload: payload})
	status := 200
	if q := f.answers[tok]; len(q) > 0 {
		status, f.answers[tok] = q[0], q[1:]
	}
	w.WriteHeader(status)
	if status != 200 {
		_, _ = io.WriteString(w, `{"reason":"`+f.reasons[status]+`"}`)
	}
}

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

func (f *fakeAPNs) provider(t *testing.T, p8, keyID string) *apns {
	t.Helper()
	p, err := newAPNS(map[string]any{"team_id": "TEAM123456", "key_id": keyID, "topic": "dev.pod32g.OmniNotify",
		"development_url": f.srv.URL, "production_url": f.srv.URL}, p8, true)
	if err != nil {
		t.Fatal(err)
	}
	return p.(*apns).withClient(f.srv.Client())
}

func page(tokens string) models.Event {
	return models.Event{
		Title: "SEV1 · jellyfin: streams failing", Summary: "#12 · level 1. Escalates in 10 min unless acknowledged.",
		Annotations: map[string]string{
			"apns_tokens": tokens, "apns_category": "INCIDENT_TRIGGERED", "apns_thread_id": "inc_1",
			"apns_interruption_level": "time-sensitive", "apns_collapse_id": "inc_1",
			"apns_data": `{"incident_id":"inc_1","incident_version":3}`,
		},
	}
}

func TestAPNsSendsAnAlertToEveryDevice(t *testing.T) {
	f, p8 := newFakeAPNs(t)
	p := f.provider(t, p8, "KEYSENDS01")
	if err := p.Send(context.Background(), models.NotificationMessage{Event: page("development:aaa, production:bbb")}); err != nil {
		t.Fatal(err)
	}
	if len(f.got) != 2 || f.got[0].token != "aaa" || f.got[1].token != "bbb" {
		t.Fatalf("requests: %+v", f.got)
	}
	if f.seenProto != "HTTP/2.0" {
		t.Errorf("protocol %s, want HTTP/2.0", f.seenProto)
	}
	r := f.got[0]
	for h, want := range map[string]string{"apns-topic": "dev.pod32g.OmniNotify", "apns-push-type": "alert",
		"apns-priority": "10", "apns-collapse-id": "inc_1"} {
		if got := r.header.Get(h); got != want {
			t.Errorf("%s = %q, want %q", h, got, want)
		}
	}
	aps := r.payload["aps"].(map[string]any)
	alert := aps["alert"].(map[string]any)
	if alert["title"] != "SEV1 · jellyfin: streams failing" || !strings.HasPrefix(alert["body"].(string), "#12") {
		t.Errorf("alert: %v", alert)
	}
	if aps["category"] != "INCIDENT_TRIGGERED" || aps["thread-id"] != "inc_1" || aps["interruption-level"] != "time-sensitive" {
		t.Errorf("aps: %v", aps)
	}
	if r.payload["incident_id"] != "inc_1" || r.payload["incident_version"] != float64(3) {
		t.Errorf("data: %v", r.payload)
	}
}

func TestAPNsRetriesAnExpiredProviderTokenOnce(t *testing.T) {
	f, p8 := newFakeAPNs(t)
	f.answers["aaa"] = []int{403}
	p := f.provider(t, p8, "KEYRETRY01")
	if err := p.Send(context.Background(), models.NotificationMessage{Event: page("development:aaa")}); err != nil {
		t.Fatal(err)
	}
	if len(f.got) != 2 {
		t.Errorf("%d requests, want the refused one and its retry", len(f.got))
	}
}

func TestAPNsSkipsGoneDevicesAndFailsOnlyWhenNoneGotIt(t *testing.T) {
	f, p8 := newFakeAPNs(t)
	f.answers["gone"] = []int{410, 410}
	p := f.provider(t, p8, "KEYGONE001")
	if err := p.Send(context.Background(), models.NotificationMessage{Event: page("development:gone,development:ok")}); err != nil {
		t.Errorf("one device reached: %v", err)
	}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: page("development:gone")}); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Errorf("every device gone: %v", err)
	}
	f.answers["down"] = []int{500}
	if err := p.Send(context.Background(), models.NotificationMessage{Event: page("development:down")}); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("Apple failing: %v", err)
	}
}

func TestAPNsRefusesBrokenConfigAndEvents(t *testing.T) {
	_, p8 := newFakeAPNs(t)
	for name, c := range map[string]struct {
		cfg    map[string]any
		secret string
	}{
		"no team":  {map[string]any{"key_id": "K", "topic": "t"}, p8},
		"no topic": {map[string]any{"team_id": "T", "key_id": "K"}, p8},
		"no key":   {map[string]any{"team_id": "T", "key_id": "K", "topic": "t"}, ""},
		"not PEM":  {map[string]any{"team_id": "T", "key_id": "K", "topic": "t"}, "nope"},
	} {
		if _, err := newAPNS(c.cfg, c.secret, false); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A .p8 whose line breaks were folded (YAML, env) or its bare body still reads.
	body := strings.Join(strings.Split(strings.TrimSpace(p8), "\n")[1:len(strings.Split(strings.TrimSpace(p8), "\n"))-1], "")
	for name, secret := range map[string]string{"folded": strings.ReplaceAll(p8, "\n", " "), "bare": body} {
		if _, err := newAPNS(map[string]any{"team_id": "T", "key_id": "K", "topic": "t"}, secret, false); err != nil {
			t.Errorf("%s key: %v", name, err)
		}
	}
	f, p8 := newFakeAPNs(t)
	p := f.provider(t, p8, "KEYBROKEN1")
	if err := p.Send(context.Background(), models.NotificationMessage{Event: models.Event{Title: "x"}}); err == nil {
		t.Error("an event naming no devices was sent")
	}
	ev := page("development:aaa")
	ev.Annotations["apns_interruption_level"] = "loud"
	if err := p.Send(context.Background(), models.NotificationMessage{Event: ev}); err == nil {
		t.Error("an unknown interruption level was sent")
	}
}

type stubProviders map[string]models.ProviderConfig

func (s stubProviders) GetProvider(_ context.Context, name string) (models.ProviderConfig, error) {
	p, ok := s[name]
	if !ok {
		return models.ProviderConfig{}, errors.New("not found")
	}
	return p, nil
}

// A second app's provider signs with the first one's key (key_from) and keeps
// its own topic; anything that is not the same Apple key is refused.
func TestAPNsBorrowsAnotherProvidersKey(t *testing.T) {
	f, p8 := newFakeAPNs(t)
	src := stubProviders{
		"oncall-push": {Name: "oncall-push", Kind: "apns", Secret: p8,
			Config: map[string]any{"team_id": "TEAM123456", "key_id": "KEYBORROW1", "topic": "dev.pod32g.OmniNotify"}},
		"borrower": {Name: "borrower", Kind: "apns",
			Config: map[string]any{"team_id": "TEAM123456", "key_id": "KEYBORROW1", "topic": "x", "key_from": "oncall-push"}},
		"Telegram": {Name: "Telegram", Kind: "telegram", Secret: "bot-token"},
	}
	r := NewDefault(nil, true, WithProviderSource(src))
	cfg := func(extra map[string]any) map[string]any {
		c := map[string]any{"team_id": "TEAM123456", "key_id": "KEYBORROW1", "topic": "dev.pod32g.OmniSentinel",
			"key_from": "oncall-push", "development_url": f.srv.URL, "production_url": f.srv.URL}
		for k, v := range extra {
			c[k] = v
		}
		return c
	}
	p, err := r.Build("apns", cfg(nil), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.(*apns).withClient(f.srv.Client()).Send(context.Background(), models.NotificationMessage{Event: page("development:aaa")}); err != nil {
		t.Fatal(err)
	}
	if len(f.got) != 1 || f.got[0].header.Get("apns-topic") != "dev.pod32g.OmniSentinel" {
		t.Fatalf("requests: %+v", f.got)
	}
	for name, c := range map[string]struct {
		cfg    map[string]any
		secret string
	}{
		"other key id":   {cfg(map[string]any{"key_id": "OTHERKEY01"}), ""},
		"other team":     {cfg(map[string]any{"team_id": "OTHERTEAM1"}), ""},
		"not apns":       {cfg(map[string]any{"key_from": "Telegram"}), ""},
		"missing":        {cfg(map[string]any{"key_from": "nope"}), ""},
		"borrowed twice": {cfg(map[string]any{"key_from": "borrower"}), ""},
		"and a secret":   {cfg(nil), p8},
	} {
		if _, err := r.Build("apns", c.cfg, c.secret); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := NewDefault(nil, true).Build("apns", cfg(nil), ""); err == nil {
		t.Error("key_from without a provider store: accepted")
	}
}
