package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/pod32g/omni-notify/internal/models"
)

// pushover posts messages to the Pushover messages API.
//
// secret: the application API token (required).
// config:
//   - user:     the user/group key (required)
//   - priority: optional integer Pushover priority (-2..2); sent only when set
//   - device:   optional target device name; sent only when set
//   - api_url:  optional endpoint override (default
//     "https://api.pushover.net/1/messages.json"), mainly for testing
type pushover struct {
	client      *http.Client
	url         string
	token       string
	user        string
	priority    int
	hasPriority bool
	device      string
}

func newPushover(client *http.Client, cfg map[string]any, secret string, allowPrivate bool) (Provider, error) {
	if secret == "" {
		return nil, fmt.Errorf("pushover: application API token secret is required")
	}
	user := cfgString(cfg, "user")
	if user == "" {
		return nil, fmt.Errorf("pushover: user (user/group key) is required")
	}
	apiURL := cfgStringDefault(cfg, "api_url", "https://api.pushover.net/1/messages.json")
	if err := validateHTTPURL(apiURL, allowPrivate); err != nil {
		return nil, fmt.Errorf("pushover: %w", err)
	}
	p := &pushover{
		client: client,
		url:    apiURL,
		token:  secret,
		user:   user,
		device: cfgString(cfg, "device"),
	}
	if cfg != nil {
		if _, ok := cfg["priority"]; ok {
			p.priority = cfgInt(cfg, "priority", 0)
			p.hasPriority = true
		}
	}
	return p, nil
}

func (p *pushover) Send(ctx context.Context, msg models.NotificationMessage) error {
	ev := msg.Event
	// Pushover requires application/x-www-form-urlencoded parameters; the ".json"
	// in the endpoint only selects the response format, not the request body.
	form := url.Values{}
	form.Set("token", p.token)
	form.Set("user", p.user)
	form.Set("title", truncate(subjectLine(ev), 250))
	form.Set("message", truncate(plainBody(ev), 1024))
	if p.device != "" {
		form.Set("device", p.device)
	}
	if p.hasPriority {
		form.Set("priority", strconv.Itoa(p.priority))
	}
	return postBytes(ctx, p.client, http.MethodPost, p.url, "application/x-www-form-urlencoded", []byte(form.Encode()), nil)
}
