package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/pod32g/omni-notify/internal/models"
)

// opsgenie creates and closes alerts via the Opsgenie Alert API v2.
//
// secret: the Opsgenie API key (required), sent as "GenieKey <key>".
// config:
//   - api_url: the alerts endpoint (optional, default
//     "https://api.opsgenie.com/v2/alerts"). Override for EU/sandbox regions
//     or testing.
//
// The alert alias is the event Fingerprint (falling back to EventID), so a
// resolved event closes the matching open alert by alias.
type opsgenie struct {
	client *http.Client
	apiURL string
	apiKey string
}

const opsgenieDefaultAPIURL = "https://api.opsgenie.com/v2/alerts"

func newOpsgenie(client *http.Client, cfg map[string]any, secret string, allowPrivate bool) (Provider, error) {
	if secret == "" {
		return nil, fmt.Errorf("opsgenie: API key secret is required")
	}
	apiURL := cfgStringDefault(cfg, "api_url", opsgenieDefaultAPIURL)
	if err := validateHTTPURL(apiURL, allowPrivate); err != nil {
		return nil, fmt.Errorf("opsgenie: %w", err)
	}
	return &opsgenie{client: client, apiURL: apiURL, apiKey: secret}, nil
}

type opsgenieCreate struct {
	Message     string `json:"message"`
	Description string `json:"description,omitempty"`
	Alias       string `json:"alias,omitempty"`
	Source      string `json:"source,omitempty"`
}

type opsgenieClose struct {
	Source string `json:"source"`
}

func (o *opsgenie) Send(ctx context.Context, msg models.NotificationMessage) error {
	ev := msg.Event
	alias := ev.Fingerprint
	if alias == "" {
		alias = ev.EventID
	}
	headers := map[string]string{"Authorization": "GenieKey " + o.apiKey}

	if ev.Status == models.StatusResolved {
		closeURL := o.apiURL + "/" + url.PathEscape(alias) + "/close?identifierType=alias"
		body, err := json.Marshal(opsgenieClose{Source: "omni-notify"})
		if err != nil {
			return fmt.Errorf("opsgenie: marshal: %w", err)
		}
		return postBytes(ctx, o.client, http.MethodPost, closeURL, "application/json", body, headers)
	}

	body, err := json.Marshal(opsgenieCreate{
		Message:     truncate(subjectLine(ev), 130),
		Description: plainBody(ev),
		Alias:       alias,
		Source:      ev.Source,
	})
	if err != nil {
		return fmt.Errorf("opsgenie: marshal: %w", err)
	}
	return postBytes(ctx, o.client, http.MethodPost, o.apiURL, "application/json", body, headers)
}
