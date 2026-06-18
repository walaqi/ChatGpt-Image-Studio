package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"chatgpt2api/internal/credential"
	"chatgpt2api/internal/identity"
)

// handleListCredentialKeys returns the current user's image-capable key
// candidates (no plaintext) so the frontend can render a key picker.
//
// Backend route: GET /api/image/credential/keys.
// Browser-facing: GET /image-studio/api/image/credential/keys.
func (s *Server) handleListCredentialKeys(w http.ResponseWriter, r *http.Request) {
	if s.credService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "credential service is not configured"})
		return
	}
	userID, ok := identity.UserIDFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "authorization is invalid"})
		return
	}

	result, err := s.credService.Resolver().ListKeys(r.Context(), userID)
	if err != nil {
		if errors.Is(err, credential.ErrUpstreamUnavailable) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "credential upstream unavailable"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// Echo the currently remembered selection so the UI can pre-highlight it.
	currentKeyID, hasCurrent, _ := s.credService.Selection().Get(r.Context(), userID)

	// Never marshal keys as JSON null: a nil slice would make the frontend's
	// result.keys.find()/.length crash. Always emit an array.
	keys := result.Keys
	if keys == nil {
		keys = []credential.KeyCandidate{}
	}

	payload := map[string]any{
		"keys":           keys,
		"can_create":     result.CanCreate,
		"image_group_id": result.ImageGroupID,
	}
	if hasCurrent {
		payload["current_key_id"] = currentKeyID
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleGetCurrentCredential returns the user's remembered key_id, if any.
//
// Backend route: GET /api/image/credential/current.
func (s *Server) handleGetCurrentCredential(w http.ResponseWriter, r *http.Request) {
	if s.credService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "credential service is not configured"})
		return
	}
	userID, ok := identity.UserIDFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "authorization is invalid"})
		return
	}

	keyID, has, err := s.credService.Selection().Get(r.Context(), userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if !has {
		writeJSON(w, http.StatusOK, map[string]any{"selected": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"selected": true, "key_id": keyID})
}

// handleSetCurrentCredential records the user's chosen key_id. The selection is
// validated (the key must resolve) before it is stored, so the picker never
// persists a key that cannot produce a credential.
//
// Backend route: PUT /api/image/credential/current.
func (s *Server) handleSetCurrentCredential(w http.ResponseWriter, r *http.Request) {
	if s.credService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "credential service is not configured"})
		return
	}
	userID, ok := identity.UserIDFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "authorization is invalid"})
		return
	}

	var body struct {
		KeyID int64 `json:"key_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	if body.KeyID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "key_id is required"})
		return
	}

	if err := s.credService.SetSelection(r.Context(), userID, body.KeyID); err != nil {
		switch {
		case errors.Is(err, credential.ErrKeyUnusable):
			writeJSON(w, http.StatusConflict, map[string]any{"error": "selected key is not usable for image generation"})
		case errors.Is(err, credential.ErrUpstreamUnavailable):
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "credential upstream unavailable"})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key_id": body.KeyID})
}
