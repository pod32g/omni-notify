package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"text/template"

	"github.com/pod32g/omni-notify/internal/models"
)

// webhook posts an event to an arbitrary HTTP endpoint.
//
// secret: either the target URL, or a JSON object carrying the URL plus an
// encrypted auth header, e.g.:
//
//	{"url":"https://host/hook","auth_header":"Authorization","auth_value":"Bearer xyz"}
//
// The JSON form keeps the auth credential encrypted at rest (the plaintext
// `headers` config below is not encrypted).
//
// config:
//   - method:       HTTP method (default POST)
//   - content_type: Content-Type header (default application/json)
//   - headers:      map of static headers to send (not encrypted)
//   - template:     optional text/template rendered with the Event; when unset
//     the raw event JSON is sent.
type webhook struct {
	client      *http.Client
	url         string
	method      string
	contentType string
	headers     map[string]string
	tmpl        *template.Template
}

// webhookSecret is the optional JSON form of a webhook secret.
type webhookSecret struct {
	URL        string `json:"url"`
	AuthHeader string `json:"auth_header"`
	AuthValue  string `json:"auth_value"`
}

func newWebhook(client *http.Client, cfg map[string]any, secret string, allowPrivate bool) (Provider, error) {
	if secret == "" {
		return nil, fmt.Errorf("webhook: target URL secret is required")
	}
	targetURL, authHeader, authValue, err := parseWebhookSecret(secret)
	if err != nil {
		return nil, fmt.Errorf("webhook: %w", err)
	}
	if err := validateHTTPURL(targetURL, allowPrivate); err != nil {
		return nil, fmt.Errorf("webhook: %w", err)
	}

	headers := cfgStringMap(cfg, "headers")
	if authHeader != "" {
		if headers == nil {
			headers = map[string]string{}
		}
		headers[authHeader] = authValue
	}

	w := &webhook{
		client:      client,
		url:         targetURL,
		method:      cfgStringDefault(cfg, "method", http.MethodPost),
		contentType: cfgStringDefault(cfg, "content_type", "application/json"),
		headers:     headers,
	}
	if tpl := cfgString(cfg, "template"); tpl != "" {
		t, err := template.New("webhook").Parse(tpl)
		if err != nil {
			return nil, fmt.Errorf("webhook: invalid template: %w", err)
		}
		w.tmpl = t
	}
	return w, nil
}

// parseWebhookSecret extracts the URL and optional auth header from a secret
// that is either a bare URL or the JSON webhookSecret form.
func parseWebhookSecret(secret string) (url, authHeader, authValue string, err error) {
	trimmed := strings.TrimSpace(secret)
	if strings.HasPrefix(trimmed, "{") {
		var ws webhookSecret
		if err := json.Unmarshal([]byte(trimmed), &ws); err != nil {
			return "", "", "", fmt.Errorf("invalid JSON secret: %w", err)
		}
		if ws.URL == "" {
			return "", "", "", fmt.Errorf("JSON secret must include a url")
		}
		if ws.AuthValue != "" && ws.AuthHeader == "" {
			return "", "", "", fmt.Errorf("auth_value set without auth_header")
		}
		return ws.URL, ws.AuthHeader, ws.AuthValue, nil
	}
	return secret, "", "", nil
}

func (w *webhook) Send(ctx context.Context, msg models.NotificationMessage) error {
	var body []byte
	if w.tmpl != nil {
		var buf bytes.Buffer
		if err := w.tmpl.Execute(&buf, msg.Event); err != nil {
			return fmt.Errorf("webhook: render template: %w", err)
		}
		body = buf.Bytes()
	} else {
		b, err := json.Marshal(msg.Event)
		if err != nil {
			return fmt.Errorf("webhook: marshal: %w", err)
		}
		body = b
	}
	return postBytes(ctx, w.client, w.method, w.url, w.contentType, body, w.headers)
}
