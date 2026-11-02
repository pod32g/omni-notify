package voice

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pod32g/omni-notify/internal/config"
	"github.com/pod32g/omni-notify/internal/models"
)

type apnsRequest struct {
	proto  string
	path   string
	header http.Header
	body   map[string]any
}

func newFakeAPNs(t *testing.T, respond func(n int, r *http.Request) (int, string)) (*httptest.Server, *[]apnsRequest) {
	t.Helper()
	var (
		mu   sync.Mutex
		reqs []apnsRequest
	)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		reqs = append(reqs, apnsRequest{proto: r.Proto, path: r.URL.Path, header: r.Header.Clone(), body: body})
		n := len(reqs)
		mu.Unlock()
		code, reason := respond(n, r)
		w.WriteHeader(code)
		if reason != "" {
			_, _ = w.Write([]byte(`{"reason":"` + reason + `"}`))
		}
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func testKey(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	return k, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestAPNSVoIPPush(t *testing.T) {
	key, pemKey := testKey(t)
	srv, reqs := newFakeAPNs(t, func(int, *http.Request) (int, string) { return 200, "" })
	a, err := NewAPNS(config.APNSConfig{KeyID: "KEY123", TeamID: "LVZHYJD66T", Key: pemKey, Topic: "dev.pod32g.OmniNotify",
		DevelopmentURL: srv.URL}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(45 * time.Second).Truncate(time.Second)
	inv := Invitation{CallID: "c1", CallerName: "Claude", CallerID: "claude", ExpiresAt: exp, RingSeconds: 30}
	if err := a.Push(context.Background(), models.Device{ID: "d1", VoIPToken: "abcd", APNSEnvironment: "development"}, inv); err != nil {
		t.Fatal(err)
	}
	r := (*reqs)[0]
	if r.proto != "HTTP/2.0" {
		t.Errorf("proto = %s, want HTTP/2.0", r.proto)
	}
	if r.path != "/3/device/abcd" {
		t.Errorf("path = %s", r.path)
	}
	for h, want := range map[string]string{
		"apns-topic": "dev.pod32g.OmniNotify.voip", "apns-push-type": "voip", "apns-priority": "10",
		"apns-expiration": "", // checked below
	} {
		if want != "" && r.header.Get(h) != want {
			t.Errorf("%s = %q, want %q", h, r.header.Get(h), want)
		}
	}
	if r.header.Get("apns-expiration") == "" || r.header.Get("apns-id") == "" {
		t.Errorf("missing apns-expiration or apns-id: %v", r.header)
	}
	tok := strings.TrimPrefix(r.header.Get("authorization"), "bearer ")
	if !verifyES256(&key.PublicKey, tok) {
		t.Error("provider token signature does not verify")
	}
	if r.body["call_id"] != "c1" || r.body["caller_name"] != "Claude" || r.body["aps"] == nil {
		t.Errorf("payload = %v", r.body)
	}
	for _, forbidden := range []string{"message", "device_token", "secret", "token"} {
		if _, ok := r.body[forbidden]; ok {
			t.Errorf("payload carries %q", forbidden)
		}
	}
}

func TestAPNSErrors(t *testing.T) {
	_, pemKey := testKey(t)
	srv, reqs := newFakeAPNs(t, func(n int, r *http.Request) (int, string) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/gone"):
			return 410, "Unregistered"
		case strings.HasSuffix(r.URL.Path, "/expired") && n == 1:
			return 403, "ExpiredProviderToken"
		case strings.HasSuffix(r.URL.Path, "/expired"):
			return 200, ""
		default:
			return 500, "InternalServerError"
		}
	})
	a, err := NewAPNS(config.APNSConfig{KeyID: "K", TeamID: "T", Key: pemKey, Topic: "x", ProductionURL: srv.URL}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	prod := func(tok string) models.Device {
		return models.Device{ID: "d", VoIPToken: tok, APNSEnvironment: "production"}
	}
	inv := Invitation{CallID: "c", ExpiresAt: time.Now().Add(time.Minute)}
	if err := a.Push(context.Background(), prod("expired"), inv); err != nil {
		t.Fatalf("expired provider token should be refreshed and retried: %v", err)
	}
	if len(*reqs) != 2 || (*reqs)[0].header.Get("authorization") == (*reqs)[1].header.Get("authorization") {
		t.Fatal("retry did not use a fresh provider token")
	}
	if err := a.Push(context.Background(), prod("gone"), inv); !errors.Is(err, ErrBadDeviceToken) {
		t.Fatalf("410: %v", err)
	}
	if err := a.Push(context.Background(), prod("other"), inv); err == nil || errors.Is(err, ErrBadDeviceToken) {
		t.Fatalf("500: %v", err)
	}
	if _, err := NewAPNS(config.APNSConfig{KeyID: "K", Key: "not a key"}, nil); err == nil {
		t.Fatal("bad key accepted")
	}
}
