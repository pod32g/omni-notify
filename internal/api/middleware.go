package api

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"time"
)

// securityHeaders sets conservative response headers. The admin UI is an
// unauthenticated shell that drives privileged API calls with a stored token, so
// it gets defense-in-depth: a self-only CSP (assets use no inline script/style),
// no framing (clickjacking), and no MIME sniffing. Harmless for API/JSON too.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// recoverMiddleware converts panics into 500s instead of crashing the server.
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic in handler", "err", rec, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the response status for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// logMiddleware logs each request at debug/info level. It never logs the
// Authorization header or request bodies, keeping credentials out of logs.
func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", clientIP(r),
		)
	})
}

// maxBodyMiddleware bounds the request body size for all endpoints.
func (s *Server) maxBodyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// authMiddleware enforces bearer-token auth on all paths except /healthz and
// (unless configured otherwise) /metrics.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.isExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if !s.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="omni-notify"`)
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isExempt reports whether a path bypasses authentication. The UI shell ("/" and
// its static assets) and /healthz are always open; the JS calls the authenticated
// API with a bearer token. /metrics is open unless configured otherwise.
func (s *Server) isExempt(path string) bool {
	if path == "/healthz" || path == "/" || strings.HasPrefix(path, "/assets/") {
		return true
	}
	if path == "/metrics" && !s.cfg.MetricsRequireAuth {
		return true
	}
	return false
}

// authorized performs a constant-time comparison of the presented bearer token
// against the configured tokens.
func (s *Server) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	presented := []byte(strings.TrimSpace(h[len(prefix):]))
	if len(presented) == 0 {
		return false
	}
	ok := false
	for _, t := range s.cfg.Tokens {
		if subtle.ConstantTimeCompare(presented, []byte(t)) == 1 {
			ok = true
		}
	}
	return ok
}

// clientIP returns the connecting peer's host for logging. It deliberately uses
// the real RemoteAddr rather than the client-spoofable X-Forwarded-For header.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
