package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/storage"
)

// handleCreateEvent ingests a single event. The raw body is retained for audit
// only; the parsed-and-normalized event is what is validated, processed, and
// delivered (so providers and retries always send the normalized form).
func (s *Server) handleCreateEvent(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}
	var ev models.Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	ev.Normalize() // migrate legacy status values before validation
	if err := ev.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.notifier.Process(r.Context(), ev, raw)
	if err != nil {
		s.log.Error("process event", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to process event")
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

// handleListEvents returns stored events with optional filters.
func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := storage.EventFilter{
		Source:      q.Get("source"),
		Type:        q.Get("type"),
		Severity:    q.Get("severity"),
		Status:      q.Get("status"),
		Fingerprint: q.Get("fingerprint"),
		Limit:       atoiDefault(q.Get("limit"), 100),
		Offset:      atoiDefault(q.Get("offset"), 0),
	}
	events, err := s.store.ListEvents(r.Context(), filter)
	if err != nil {
		s.log.Error("list events", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to list events")
		return
	}
	if events == nil {
		events = []models.StoredEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "count": len(events)})
}

// handleGetEvent returns a single event by numeric id.
func (s *Server) handleGetEvent(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid event id")
		return
	}
	ev, err := s.store.GetEvent(r.Context(), id)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "event not found")
		return
	}
	if err != nil {
		s.log.Error("get event", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to get event")
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

// atoiDefault parses s as an int, returning def on error/empty.
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
