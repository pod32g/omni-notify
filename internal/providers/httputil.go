package providers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// validateHTTPURL ensures a provider target is a well-formed http/https URL.
// Non-HTTP schemes (file://, gopher://, etc.) are always rejected. When
// allowPrivate is false, literal private/loopback/link-local/multicast hosts and
// "localhost" are rejected at build time; hostnames that resolve to private IPs
// are caught at connection time by the guarded dialer (see NewGuardedClient).
func validateHTTPURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported URL scheme %q (only http/https allowed)", u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("URL must include a host")
	}
	if !allowPrivate && isDisallowedHost(u.Hostname()) {
		return fmt.Errorf("target host %q is private/loopback and allow_private_webhook_targets is false", u.Hostname())
	}
	return nil
}

// isDisallowedHost reports whether a host literal should be blocked when private
// targets are not allowed. Bare hostnames (other than localhost) return false
// here; they are validated after DNS resolution by the guarded dialer.
func isDisallowedHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return isPrivateIP(ip)
	}
	return false
}

// extraBlockedCIDRs covers ranges the stdlib net.IP predicates do not classify:
// RFC 6598 carrier-grade NAT and RFC 6052 NAT64 (which both reach internal IPv4).
var extraBlockedCIDRs = func() []*net.IPNet {
	var nets []*net.IPNet
	for _, c := range []string{"100.64.0.0/10", "64:ff9b::/96"} {
		if _, n, err := net.ParseCIDR(c); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}()

// isPrivateIP reports whether ip is loopback, private (RFC1918/ULA), link-local,
// multicast, unspecified, CGNAT (RFC 6598) or NAT64 (RFC 6052). IPv4-mapped IPv6
// addresses are normalised so they cannot bypass the IPv4 checks.
func isPrivateIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, n := range extraBlockedCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// NewGuardedClient builds an *http.Client whose dialer rejects connections to
// private/loopback/link-local/multicast addresses when allowPrivate is false.
// The check runs after DNS resolution, defeating hostname-based SSRF and DNS
// rebinding.
func NewGuardedClient(timeout time.Duration, allowPrivate bool) *http.Client {
	control := func(network, address string, _ syscall.RawConn) error {
		if allowPrivate {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip != nil && isPrivateIP(ip) {
			return fmt.Errorf("blocked connection to private address %s", host)
		}
		return nil
	}
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 15 * time.Second,
			Control: control,
		}).DialContext,
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

// cfgString returns a string config value, or "" if missing/not a string.
func cfgString(cfg map[string]any, key string) string {
	if cfg == nil {
		return ""
	}
	if v, ok := cfg[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// cfgStringDefault returns a string config value or def if missing.
func cfgStringDefault(cfg map[string]any, key, def string) string {
	if s := cfgString(cfg, key); s != "" {
		return s
	}
	return def
}

// cfgInt returns an int config value (YAML/JSON numbers arrive as int/float64).
func cfgInt(cfg map[string]any, key string, def int) int {
	if cfg == nil {
		return def
	}
	switch v := cfg[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return def
	}
}

// cfgStringSlice returns a []string config value, accepting []any or []string.
func cfgStringSlice(cfg map[string]any, key string) []string {
	if cfg == nil {
		return nil
	}
	switch v := cfg[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		// allow comma-separated
		parts := strings.Split(v, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	default:
		return nil
	}
}

// cfgStringMap returns a map[string]string config value.
func cfgStringMap(cfg map[string]any, key string) map[string]string {
	if cfg == nil {
		return nil
	}
	out := map[string]string{}
	switch v := cfg[key].(type) {
	case map[string]string:
		return v
	case map[string]any:
		for k, val := range v {
			if s, ok := val.(string); ok {
				out[k] = s
			}
		}
	default:
		return nil
	}
	return out
}

// postBytes sends body to url with the given content type and headers, treating
// any non-2xx response as an error that includes a snippet of the response body.
func postBytes(ctx context.Context, client *http.Client, method, url, contentType string, body []byte, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return nil
}
