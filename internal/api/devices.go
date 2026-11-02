package api

import (
	"errors"
	"net/http"

	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/storage"
	"github.com/pod32g/omni-notify/internal/voice"
)

// handleEnrollDevice redeems a one-time code (unauthenticated: the code is
// the credential) and returns the device's own credential, once.
func (s *Server) handleEnrollDevice(w http.ResponseWriter, r *http.Request) {
	var req voice.EnrollRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	d, secret, err := s.cfg.Voice.Enroll(r.Context(), req)
	if err != nil {
		s.writeVoiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"device":       d,
		"device_token": secret,
		"push":         s.cfg.Voice.PushConfigured(),
		"ring_seconds": int(s.cfg.Voice.Config().DeviceRingTimeout.D().Seconds()),
	})
}

func (s *Server) handleCreateEnrollment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User string `json:"user"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	code, expires, err := s.cfg.Voice.CreateEnrollment(r.Context(), req.User)
	if err != nil {
		s.writeVoiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"code": code, "expires_at": expires, "user": req.User})
}

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := s.store.ListDevices(r.Context(), r.URL.Query().Get("user"), false)
	if err != nil {
		s.writeVoiceError(w, err)
		return
	}
	if devices == nil {
		devices = []models.Device{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices, "count": len(devices)})
}

func (s *Server) handlePatchDevice(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Disabled *bool `json:"disabled"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Disabled == nil {
		writeError(w, http.StatusBadRequest, "disabled is required")
		return
	}
	id := r.PathValue("id")
	if err := s.store.SetDeviceDisabled(r.Context(), id, *req.Disabled); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "device not found")
			return
		}
		s.writeVoiceError(w, err)
		return
	}
	d, err := s.store.GetDevice(r.Context(), id)
	if err != nil {
		s.writeVoiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleGetSelf(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.kind != principalDevice {
		writeError(w, http.StatusForbidden, "device credential required")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device":       p.device,
		"push":         s.cfg.Voice.PushConfigured(),
		"ring_seconds": int(s.cfg.Voice.Config().DeviceRingTimeout.D().Seconds()),
	})
}

func (s *Server) handlePutSelf(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.kind != principalDevice {
		writeError(w, http.StatusForbidden, "device credential required")
		return
	}
	var req struct {
		VoIPToken       string `json:"voip_token"`
		APNSEnvironment string `json:"apns_environment"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.cfg.Voice.UpdatePush(r.Context(), p.device, req.VoIPToken, req.APNSEnvironment); err != nil {
		s.writeVoiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRinging(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.kind != principalDevice {
		writeError(w, http.StatusForbidden, "device credential required")
		return
	}
	inv, err := s.cfg.Voice.Ringing(r.Context(), p.device)
	if err != nil {
		s.writeVoiceError(w, err)
		return
	}
	type invitation struct {
		voice.Invitation
		ExpiresAt int64 `json:"expires_at"`
	}
	out := make([]invitation, 0, len(inv))
	for _, i := range inv {
		out = append(out, invitation{Invitation: i, ExpiresAt: i.ExpiresAt.Unix()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"calls": out})
}
