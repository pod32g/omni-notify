// Package api exposes Omni-Notify's REST endpoints, middleware and HTTP server.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// errorResponse is the JSON body returned for any error.
type errorResponse struct {
	Error string `json:"error"`
}

// writeJSON writes v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		slog.Default().Error("write json response", "err", err)
	}
}

// writeError writes a JSON error with the given status code.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}
