package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/storage"
)

// providerRequest is the POST/PUT body for a provider. An empty secret leaves
// the stored secret unchanged (secrets are never readable back).
type providerRequest struct {
	Name    string         `json:"name"`
	Kind    string         `json:"kind"`
	Config  map[string]any `json:"config"`
	Secret  string         `json:"secret"`
	Enabled *bool          `json:"enabled"`
}

// providerPatch is the PATCH body: every field is optional and only supplied
// fields are changed.
type providerPatch struct {
	Kind    *string        `json:"kind"`
	Config  map[string]any `json:"config"`
	Secret  *string        `json:"secret"`
	Enabled *bool          `json:"enabled"`
}

// providerResponse is the masked, API-safe view of a provider.
type providerResponse struct {
	Name      string           `json:"name"`
	Kind      string           `json:"kind"`
	Config    map[string]any   `json:"config,omitempty"`
	Enabled   bool             `json:"enabled"`
	HasSecret bool             `json:"has_secret"`
	ManagedBy models.ManagedBy `json:"managed_by"`
	CreatedAt string           `json:"created_at,omitempty"`
	UpdatedAt string           `json:"updated_at,omitempty"`
}

func toProviderResponse(p models.ProviderConfig) providerResponse {
	resp := providerResponse{
		Name:      p.Name,
		Kind:      p.Kind,
		Config:    p.Config,
		Enabled:   p.Enabled,
		HasSecret: p.HasSecret(),
		ManagedBy: p.ManagedBy,
	}
	if !p.CreatedAt.IsZero() {
		resp.CreatedAt = p.CreatedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	if !p.UpdatedAt.IsZero() {
		resp.UpdatedAt = p.UpdatedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	return resp
}

// handleListProviders returns all providers with secrets masked.
func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	provs, err := s.store.ListProviders(r.Context())
	if err != nil {
		s.log.Error("list providers", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to list providers")
		return
	}
	out := make([]providerResponse, 0, len(provs))
	for _, p := range provs {
		out = append(out, toProviderResponse(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out, "count": len(out)})
}

// handleGetProvider returns a single provider (masked).
func (s *Server) handleGetProvider(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetProvider(r.Context(), r.PathValue("name"))
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	if err != nil {
		s.log.Error("get provider", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to get provider")
		return
	}
	writeJSON(w, http.StatusOK, toProviderResponse(p))
}

// handleCreateProvider creates a new provider; it returns 409 if one already
// exists with that name (use PUT/PATCH to modify).
func (s *Server) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
	var req providerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if msg, ok := s.validateProviderKind(req.Kind); !ok {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	exists, err := s.store.ProviderExists(r.Context(), req.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check provider")
		return
	}
	if exists {
		writeError(w, http.StatusConflict, "provider already exists: "+req.Name)
		return
	}
	s.writeProvider(w, r, models.ProviderConfig{
		Name:      req.Name,
		Kind:      req.Kind,
		Config:    req.Config,
		Secret:    req.Secret,
		Enabled:   enabledOrDefault(req.Enabled, true),
		ManagedBy: models.ManagedByAPI,
	}, http.StatusCreated)
}

// handleReplaceProvider replaces a provider's representation (PUT). It creates
// the provider if it does not yet exist. An omitted secret keeps the existing one.
func (s *Server) handleReplaceProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req providerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if msg, ok := s.validateProviderKind(req.Kind); !ok {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	status := http.StatusOK
	if exists, err := s.store.ProviderExists(r.Context(), name); err == nil && !exists {
		status = http.StatusCreated
	}
	s.writeProvider(w, r, models.ProviderConfig{
		Name:      name,
		Kind:      req.Kind,
		Config:    req.Config,
		Secret:    req.Secret,
		Enabled:   enabledOrDefault(req.Enabled, true),
		ManagedBy: models.ManagedByAPI,
	}, status)
}

// handlePatchProvider partially updates an existing provider (PATCH).
func (s *Server) handlePatchProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	existing, err := s.store.GetProvider(r.Context(), name)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load provider")
		return
	}
	var patch providerPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if patch.Kind != nil {
		if msg, ok := s.validateProviderKind(*patch.Kind); !ok {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		existing.Kind = *patch.Kind
	}
	if patch.Config != nil {
		if existing.Config == nil {
			existing.Config = map[string]any{}
		}
		for k, v := range patch.Config {
			existing.Config[k] = v
		}
	}
	if patch.Enabled != nil {
		existing.Enabled = *patch.Enabled
	}
	existing.Secret = "" // keep existing unless a new one is supplied
	if patch.Secret != nil && *patch.Secret != "" {
		existing.Secret = *patch.Secret
	}
	existing.ManagedBy = models.ManagedByAPI
	s.writeProvider(w, r, existing, http.StatusOK)
}

// writeProvider validates that the provider is constructible (valid URL/scheme,
// SSRF policy, required fields), persists it, and writes the masked result.
func (s *Server) writeProvider(w http.ResponseWriter, r *http.Request, p models.ProviderConfig, status int) {
	// Validate against the effective secret: the supplied one, or the stored one
	// when the caller omitted it (PUT/PATCH secret preservation).
	effectiveSecret := p.Secret
	if effectiveSecret == "" {
		if existing, err := s.store.GetProvider(r.Context(), p.Name); err == nil {
			effectiveSecret = existing.Secret
		}
	}
	if _, err := s.registry.Build(p.Kind, p.Config, effectiveSecret); err != nil {
		writeError(w, http.StatusBadRequest, "invalid provider config: "+err.Error())
		return
	}
	if err := s.store.UpsertProvider(r.Context(), p); err != nil {
		s.log.Error("save provider", "err", err, "name", p.Name)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	stored, err := s.store.GetProvider(r.Context(), p.Name)
	if err != nil {
		s.log.Error("reload provider", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to load provider")
		return
	}
	writeJSON(w, status, toProviderResponse(stored))
}

// validateProviderKind checks the kind is present and registered.
func (s *Server) validateProviderKind(kind string) (string, bool) {
	if kind == "" {
		return "kind is required", false
	}
	if !s.registry.Has(kind) {
		return "unknown provider kind: " + kind, false
	}
	return "", true
}

func enabledOrDefault(p *bool, def bool) bool {
	if p != nil {
		return *p
	}
	return def
}
