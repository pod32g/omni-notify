package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/storage"
)

// routeRequest is the POST/PUT body for a route.
type routeRequest struct {
	Name           string            `json:"name"`
	Match          map[string]string `json:"match"`
	Providers      []string          `json:"providers"`
	IsDefault      bool              `json:"is_default"`
	Disabled       bool              `json:"disabled"`
	Priority       int               `json:"priority"`
	StopProcessing bool              `json:"stop_processing"`
	DedupWindow    models.Duration   `json:"dedup_window"`
	RepeatInterval models.Duration   `json:"repeat_interval"`
}

// routePatch is the PATCH body: only supplied fields are changed.
type routePatch struct {
	Match          map[string]string `json:"match"`
	Providers      []string          `json:"providers"`
	IsDefault      *bool             `json:"is_default"`
	Disabled       *bool             `json:"disabled"`
	Priority       *int              `json:"priority"`
	StopProcessing *bool             `json:"stop_processing"`
	DedupWindow    *models.Duration  `json:"dedup_window"`
	RepeatInterval *models.Duration  `json:"repeat_interval"`
}

// handleListRoutes returns all routes.
func (s *Server) handleListRoutes(w http.ResponseWriter, r *http.Request) {
	routes, err := s.store.ListRoutes(r.Context())
	if err != nil {
		s.log.Error("list routes", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to list routes")
		return
	}
	if routes == nil {
		routes = []models.Route{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"routes": routes, "count": len(routes)})
}

// handleGetRoute returns a single route.
func (s *Server) handleGetRoute(w http.ResponseWriter, r *http.Request) {
	route, err := s.store.GetRoute(r.Context(), r.PathValue("name"))
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "route not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get route")
		return
	}
	writeJSON(w, http.StatusOK, route)
}

// handleCreateRoute creates a new route; 409 if the name is taken.
func (s *Server) handleCreateRoute(w http.ResponseWriter, r *http.Request) {
	var req routeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if !s.validateRouteProviders(w, r, req.Providers) {
		return
	}
	exists, err := s.store.RouteExists(r.Context(), req.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check route")
		return
	}
	if exists {
		writeError(w, http.StatusConflict, "route already exists: "+req.Name)
		return
	}
	s.writeRoute(w, r, routeFromRequest(req.Name, req), http.StatusCreated)
}

// handleReplaceRoute replaces a route (PUT), creating it if absent.
func (s *Server) handleReplaceRoute(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req routeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if !s.validateRouteProviders(w, r, req.Providers) {
		return
	}
	status := http.StatusOK
	if exists, err := s.store.RouteExists(r.Context(), name); err == nil && !exists {
		status = http.StatusCreated
	}
	s.writeRoute(w, r, routeFromRequest(name, req), status)
}

// handlePatchRoute partially updates an existing route (PATCH).
func (s *Server) handlePatchRoute(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	route, err := s.store.GetRoute(r.Context(), name)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "route not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load route")
		return
	}
	var patch routePatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if patch.Providers != nil {
		if !s.validateRouteProviders(w, r, patch.Providers) {
			return
		}
		route.Providers = patch.Providers
	}
	if patch.Match != nil {
		route.Match = patch.Match
	}
	if patch.IsDefault != nil {
		route.IsDefault = *patch.IsDefault
	}
	if patch.Disabled != nil {
		route.Disabled = *patch.Disabled
	}
	if patch.Priority != nil {
		route.Priority = *patch.Priority
	}
	if patch.StopProcessing != nil {
		route.StopProcessing = *patch.StopProcessing
	}
	if patch.DedupWindow != nil {
		route.DedupWindow = *patch.DedupWindow
	}
	if patch.RepeatInterval != nil {
		route.RepeatInterval = *patch.RepeatInterval
	}
	route.ManagedBy = models.ManagedByAPI
	s.writeRoute(w, r, route, http.StatusOK)
}

func routeFromRequest(name string, req routeRequest) models.Route {
	return models.Route{
		Name:           name,
		Match:          req.Match,
		Providers:      req.Providers,
		IsDefault:      req.IsDefault,
		Disabled:       req.Disabled,
		Priority:       req.Priority,
		StopProcessing: req.StopProcessing,
		DedupWindow:    req.DedupWindow,
		RepeatInterval: req.RepeatInterval,
		ManagedBy:      models.ManagedByAPI,
	}
}

// validateRouteProviders ensures at least one provider is given and all exist.
// It writes the error response and returns false on failure.
func (s *Server) validateRouteProviders(w http.ResponseWriter, r *http.Request, names []string) bool {
	if len(names) == 0 {
		writeError(w, http.StatusBadRequest, "at least one provider is required")
		return false
	}
	for _, pn := range names {
		ok, err := s.store.ProviderExists(r.Context(), pn)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to validate providers")
			return false
		}
		if !ok {
			writeError(w, http.StatusBadRequest, "unknown provider: "+pn)
			return false
		}
	}
	return true
}

// writeRoute persists route and writes the stored result with the given status.
func (s *Server) writeRoute(w http.ResponseWriter, r *http.Request, route models.Route, status int) {
	if err := s.store.UpsertRoute(r.Context(), route); err != nil {
		s.log.Error("save route", "err", err, "name", route.Name)
		writeError(w, http.StatusInternalServerError, "failed to save route")
		return
	}
	stored, err := s.store.GetRoute(r.Context(), route.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load route")
		return
	}
	writeJSON(w, status, stored)
}
