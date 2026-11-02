package voice

import (
	"context"
	"fmt"

	"github.com/pod32g/omni-notify/internal/models"
)

// RouteProvider is the voice_call provider kind: any route can deliver an
// event by phone. Its config names the recipient and the agent the call is
// placed as; both come from the admin's provider config, never from the event.
//
//	{"name": "call-me", "kind": "voice_call", "config": {"recipient": "me", "agent": "omni-notify"}}
//
// Delivery succeeds once the call is scheduled; the call then has its own
// lifecycle and outcome (GET /api/v1/calls).
type RouteProvider struct {
	svc              *Service
	recipient, agent string
}

// NewRouteProvider validates a voice_call provider's config.
func NewRouteProvider(svc *Service, cfg map[string]any) (*RouteProvider, error) {
	recipient, _ := cfg["recipient"].(string)
	agent, _ := cfg["agent"].(string)
	if recipient == "" || agent == "" {
		return nil, fmt.Errorf("voice_call: config.recipient and config.agent are required")
	}
	u, ok := svc.cfg.User(recipient)
	if !ok {
		return nil, fmt.Errorf("voice_call: unknown recipient %q", recipient)
	}
	if _, ok := svc.cfg.Agent(agent); !ok {
		return nil, fmt.Errorf("voice_call: agent %q is not in voice.agents", agent)
	}
	if !contains(u.AllowedAgents, agent) {
		return nil, fmt.Errorf("voice_call: %s may not call %s", agent, u.ID)
	}
	return &RouteProvider{svc: svc, recipient: u.ID, agent: agent}, nil
}

// Send schedules the call.
func (p *RouteProvider) Send(ctx context.Context, msg models.NotificationMessage) error {
	return p.svc.CreateFromEvent(ctx, msg, p.recipient, p.agent)
}
