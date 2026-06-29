package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/storage"
)

// handleHealth reports liveness/readiness, verifying database connectivity.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "error": "database unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleListDeliveries returns delivery history, filterable by fingerprint/status.
func (s *Server) handleListDeliveries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := storage.DeliveryFilter{
		Fingerprint: q.Get("fingerprint"),
		Status:      q.Get("status"),
		Limit:       atoiDefault(q.Get("limit"), 100),
	}
	deliveries, err := s.store.ListDeliveries(r.Context(), filter)
	if err != nil {
		s.log.Error("list deliveries", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to list deliveries")
		return
	}
	if deliveries == nil {
		deliveries = []models.Delivery{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": deliveries, "count": len(deliveries)})
}

// testRequest is the POST body for /api/v1/test.
type testRequest struct {
	Provider string `json:"provider"`
}

// handleTest sends a synthetic notification through a named provider so users can
// verify configuration end-to-end.
func (s *Server) handleTest(w http.ResponseWriter, r *http.Request) {
	var req testRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Provider == "" {
		writeError(w, http.StatusBadRequest, "provider is required")
		return
	}
	err := s.notifier.TestProvider(r.Context(), req.Provider)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "provider not found: "+req.Provider)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok":       false,
			"provider": req.Provider,
			"error":    err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "provider": req.Provider})
}
