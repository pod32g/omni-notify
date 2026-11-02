package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/pod32g/omni-notify/internal/auth"
	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/voice"
)

type principalKind int

const (
	principalAdmin principalKind = iota + 1
	principalAgent
	principalDevice
)

// principal is who a request authenticated as.
type principal struct {
	kind   principalKind
	agent  string        // principalAgent: the agent id
	via    string        // how an agent authenticated: "identity" or "token"
	device models.Device // principalDevice
}

type principalKey struct{}

func withPrincipal(ctx context.Context, p principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

func principalFrom(ctx context.Context) principal {
	p, _ := ctx.Value(principalKey{}).(principal)
	return p
}

// isVoicePath reports whether non-admin principals may reach path.
func isVoicePath(p string) bool {
	return p == "/api/v1/calls" || strings.HasPrefix(p, "/api/v1/calls/") ||
		p == "/api/v1/devices/self" || strings.HasPrefix(p, "/api/v1/devices/self/")
}

// authenticate resolves the bearer credential: an admin token; with voice
// enabled, a device credential, an agent's static token (by its SHA-256), or
// an Omni-Identity client_credentials token mapped to an agent.
func (s *Server) authenticate(r *http.Request) (principal, bool) {
	tok := bearer(r)
	if tok == "" {
		return principal{}, false
	}
	if s.isAdminToken(tok) {
		return principal{kind: principalAdmin}, true
	}
	v := s.cfg.Voice
	if v == nil {
		return principal{}, false
	}
	if strings.HasPrefix(tok, voice.DeviceTokenPrefix) {
		d, err := v.AuthenticateDevice(r.Context(), tok)
		if err != nil {
			return principal{}, false
		}
		return principal{kind: principalDevice, device: d}, true
	}
	vc := v.Config()
	sum := sha256.Sum256([]byte(tok))
	if a, ok := vc.AgentForTokenHash(hex.EncodeToString(sum[:])); ok {
		return principal{kind: principalAgent, agent: a.ID, via: "token"}, true
	}
	if s.cfg.Identity != nil && auth.LooksLikeJWT(tok) {
		claims, err := s.cfg.Identity.Verify(r.Context(), tok)
		if err != nil {
			s.log.Debug("identity token rejected", "err", err)
			return principal{}, false
		}
		client, ok := claims.MachineClient()
		if !ok {
			return principal{}, false
		}
		if a, ok := vc.AgentForClient(client); ok {
			return principal{kind: principalAgent, agent: a.ID, via: "identity"}, true
		}
	}
	return principal{}, false
}
