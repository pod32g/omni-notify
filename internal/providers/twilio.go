package providers

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/pod32g/omni-notify/internal/models"
)

// twilio sends an SMS via the Twilio REST API.
//
// secret: the Twilio auth token (required).
// config:
//   - account_sid (required): the Twilio Account SID, used in the URL and auth.
//   - from (required): the sender phone number (E.164).
//   - to (required): the recipient phone number (E.164).
//   - api_base (optional): API base URL, default "https://api.twilio.com".
type twilio struct {
	client     *http.Client
	url        string
	from       string
	to         string
	authHeader string
}

func newTwilio(client *http.Client, cfg map[string]any, secret string, allowPrivate bool) (Provider, error) {
	if secret == "" {
		return nil, fmt.Errorf("twilio: auth token secret is required")
	}
	accountSID := cfgString(cfg, "account_sid")
	if accountSID == "" {
		return nil, fmt.Errorf("twilio: account_sid is required")
	}
	from := cfgString(cfg, "from")
	if from == "" {
		return nil, fmt.Errorf("twilio: from is required")
	}
	to := cfgString(cfg, "to")
	if to == "" {
		return nil, fmt.Errorf("twilio: to is required")
	}
	apiBase := cfgStringDefault(cfg, "api_base", "https://api.twilio.com")
	if err := validateHTTPURL(apiBase, allowPrivate); err != nil {
		return nil, fmt.Errorf("twilio: %w", err)
	}
	endpoint := strings.TrimRight(apiBase, "/") + "/2010-04-01/Accounts/" + url.PathEscape(accountSID) + "/Messages.json"

	creds := base64.StdEncoding.EncodeToString([]byte(accountSID + ":" + secret))
	return &twilio{
		client:     client,
		url:        endpoint,
		from:       from,
		to:         to,
		authHeader: "Basic " + creds,
	}, nil
}

func (t *twilio) Send(ctx context.Context, msg models.NotificationMessage) error {
	ev := msg.Event
	body := subjectLine(ev)
	if ev.Summary != "" {
		body += "\n" + ev.Summary
	}
	body = truncate(body, 1500)

	form := url.Values{}
	form.Set("From", t.from)
	form.Set("To", t.to)
	form.Set("Body", body)

	headers := map[string]string{
		"Authorization": sanitizeHeader(t.authHeader),
	}
	return postBytes(ctx, t.client, http.MethodPost, t.url, "application/x-www-form-urlencoded", []byte(form.Encode()), headers)
}
