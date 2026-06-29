package api

import (
	"errors"
	"net/http"

	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/storage"
)

// handleListStates returns current event states. ?active=true filters to active.
func (s *Server) handleListStates(w http.ResponseWriter, r *http.Request) {
	activeOnly := r.URL.Query().Get("active") == "true"
	states, err := s.store.ListStates(r.Context(), activeOnly)
	if err != nil {
		s.log.Error("list states", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to list states")
		return
	}
	if states == nil {
		states = []models.State{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"states": states, "count": len(states)})
}

// handleGetState returns the current state for a fingerprint.
func (s *Server) handleGetState(w http.ResponseWriter, r *http.Request) {
	fp := r.PathValue("fingerprint")
	st, err := s.store.GetState(r.Context(), fp)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "state not found")
		return
	}
	if err != nil {
		s.log.Error("get state", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to get state")
		return
	}
	writeJSON(w, http.StatusOK, st)
}
