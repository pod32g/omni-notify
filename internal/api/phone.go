package api

import (
	"net/http"
	"strings"

	"github.com/pod32g/omni-notify/internal/voice"
)

// The phone door: a second listener for what a call in progress needs and
// nothing else (omni-notify-ios docs/calls/API.md, C5). It is published
// through Omni Access as a public name, because a locked phone can't sign in
// to Omni Access, yet must load the call it just answered, report its state
// and take turns. Only device credentials are accepted here; admin and agent
// tokens, and every other route, stay behind the gateway on the main listener.

// PhoneHandler builds the door's handler.
func (s *Server) PhoneHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	if s.cfg.Voice != nil {
		mux.HandleFunc("GET /api/v1/calls/{id}", s.handleGetCall)
		mux.HandleFunc("POST /api/v1/calls/{id}/state", s.handleCallState)
		mux.HandleFunc("POST /api/v1/calls/{id}/turns", s.handleCallTurn)
	}
	var h http.Handler = mux
	h = s.phoneAuth(h)
	h = s.maxBodyMiddleware(h)
	h = s.securityHeaders(h)
	h = s.logMiddleware(h)
	h = s.recoverMiddleware(h)
	return h
}

// phoneAuth admits a phone's device credential and nothing else.
func (s *Server) phoneAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if s.cfg.Voice == nil || !strings.HasPrefix(bearer(r), voice.DeviceTokenPrefix) {
			writeError(w, http.StatusUnauthorized, "a phone's device credential is required")
			return
		}
		p, ok := s.authenticate(r)
		if !ok || p.kind != principalDevice {
			writeError(w, http.StatusUnauthorized, "a phone's device credential is required")
			return
		}
		next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
	})
}

// PhoneServer serves the door on addr with the API's timeouts.
func (s *Server) PhoneServer(addr string) *http.Server {
	return &http.Server{Addr: addr, Handler: s.PhoneHandler(), ReadTimeout: s.cfg.ReadTimeout,
		WriteTimeout: s.cfg.WriteTimeout}
}
