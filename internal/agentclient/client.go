// Package agentclient is the agent side of the voice call API: the client the
// MCP server and the `omni-notify call` command use. It authenticates with an
// Omni-Identity client_credentials token (preferred) or a static agent token.
package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Client calls Omni-Notify's /api/v1/calls endpoints.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Tokens  TokenSource
}

// TokenSource yields a bearer token.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// StaticToken is a fixed bearer token (an agent token, or the admin token for
// simulated calls).
type StaticToken string

// Token returns the token.
func (s StaticToken) Token(context.Context) (string, error) { return string(s), nil }

// ClientCredentials fetches and caches an Omni-Identity access token.
type ClientCredentials struct {
	TokenURL, ClientID, Secret string
	HTTP                       *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// Token returns a cached token or fetches a new one a minute before expiry.
func (c *ClientCredentials) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expires) > time.Minute {
		return c.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(c.ClientID), url.QueryEscape(c.Secret))
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("identity token: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body)
	if resp.StatusCode != http.StatusOK || body.AccessToken == "" {
		return "", fmt.Errorf("identity token: %d %s", resp.StatusCode, body.Error)
	}
	c.token, c.expires = body.AccessToken, time.Now().Add(time.Duration(body.ExpiresIn)*time.Second)
	return c.token, nil
}

// FromEnv builds a client from the environment:
//
//	OMNI_NOTIFY_URL                     base URL (default http://localhost:8080)
//	OMNI_IDENTITY_TOKEN_URL             Identity token endpoint, with
//	OMNI_NOTIFY_CLIENT_ID               a confidential client mapped to the agent, and
//	OMNI_NOTIFY_CLIENT_SECRET[_FILE]    its secret
//	OMNI_NOTIFY_AGENT_TOKEN[_FILE]      or a static agent (or admin) token
func FromEnv() (*Client, error) {
	c := &Client{BaseURL: strings.TrimRight(envOr("OMNI_NOTIFY_URL", "http://localhost:8080"), "/"),
		HTTP: &http.Client{Timeout: 20 * time.Second}}
	if tokURL := os.Getenv("OMNI_IDENTITY_TOKEN_URL"); tokURL != "" {
		secret, err := secretEnv("OMNI_NOTIFY_CLIENT_SECRET")
		if err != nil {
			return nil, err
		}
		id := os.Getenv("OMNI_NOTIFY_CLIENT_ID")
		if id == "" || secret == "" {
			return nil, fmt.Errorf("OMNI_IDENTITY_TOKEN_URL needs OMNI_NOTIFY_CLIENT_ID and OMNI_NOTIFY_CLIENT_SECRET(_FILE)")
		}
		c.Tokens = &ClientCredentials{TokenURL: tokURL, ClientID: id, Secret: secret}
		return c, nil
	}
	tok, err := secretEnv("OMNI_NOTIFY_AGENT_TOKEN")
	if err != nil {
		return nil, err
	}
	if tok == "" {
		return nil, fmt.Errorf("set OMNI_IDENTITY_TOKEN_URL (+ client id/secret) or OMNI_NOTIFY_AGENT_TOKEN(_FILE)")
	}
	c.Tokens = StaticToken(tok)
	return c, nil
}

func secretEnv(name string) (string, error) {
	if f := os.Getenv(name + "_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", name, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return os.Getenv(name), nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// APIError is a non-2xx answer from Omni-Notify.
type APIError struct {
	Status int
	Body   map[string]any
}

func (e *APIError) Error() string {
	if msg, ok := e.Body["error"].(string); ok {
		return fmt.Sprintf("omni-notify %d: %s", e.Status, msg)
	}
	return fmt.Sprintf("omni-notify %d", e.Status)
}

// Do sends a JSON request and decodes the JSON answer.
func (c *Client) Do(ctx context.Context, method, path string, body any) (map[string]any, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return nil, err
	}
	tok, err := c.Tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return out, &APIError{Status: resp.StatusCode, Body: out}
	}
	return out, nil
}
