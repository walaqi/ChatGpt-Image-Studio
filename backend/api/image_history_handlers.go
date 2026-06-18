package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"chatgpt2api/internal/identity"
	"chatgpt2api/internal/imagehistory"
)

func (s *Server) handleListImageConversations(w http.ResponseWriter, r *http.Request) {
	if !s.serverImageConversationStorageEnabled() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "server image storage is disabled"})
		return
	}
	store, err := imagehistory.NewStore(s.cfg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer store.Close()

	userID, ok := identity.UserIDFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthenticated"})
		return
	}
	items, err := store.List(r.Context(), userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleGetImageConversation(w http.ResponseWriter, r *http.Request) {
	if !s.serverImageConversationStorageEnabled() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "server image storage is disabled"})
		return
	}
	store, err := imagehistory.NewStore(s.cfg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer store.Close()

	userID, ok := identity.UserIDFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthenticated"})
		return
	}
	item, err := store.Get(r.Context(), userID, r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if item == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "conversation not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"item": item})
}

func (s *Server) handleSaveImageConversation(w http.ResponseWriter, r *http.Request) {
	if !s.serverImageConversationStorageEnabled() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "server image storage is disabled"})
		return
	}
	var body imagehistory.Conversation
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	if pathID := strings.TrimSpace(r.PathValue("id")); pathID != "" {
		body.ID = pathID
	}

	userID, ok := identity.UserIDFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthenticated"})
		return
	}

	store, err := imagehistory.NewStore(s.cfg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer store.Close()

	item, err := store.Save(r.Context(), userID, body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"item": item})
}

func (s *Server) handleDeleteImageConversation(w http.ResponseWriter, r *http.Request) {
	if !s.serverImageConversationStorageEnabled() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "server image storage is disabled"})
		return
	}
	userID, ok := identity.UserIDFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthenticated"})
		return
	}

	store, err := imagehistory.NewStore(s.cfg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer store.Close()

	if err := store.Delete(r.Context(), userID, r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleClearImageConversations(w http.ResponseWriter, r *http.Request) {
	if !s.serverImageConversationStorageEnabled() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "server image storage is disabled"})
		return
	}
	userID, ok := identity.UserIDFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthenticated"})
		return
	}

	store, err := imagehistory.NewStore(s.cfg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer store.Close()

	if err := store.Clear(r.Context(), userID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleImportImageConversations imports conversation content for the current
// user. Per review #4 (docs §4.5) it accepts ONLY conversation items — never
// storage coordinates (backend/redisAddr/sqlitePath/...). The server's own
// configured store is always used, and Clear/Save are scoped to the current
// user, so a tenant can neither point the server at an arbitrary Redis/path nor
// touch another user's history.
func (s *Server) handleImportImageConversations(w http.ResponseWriter, r *http.Request) {
	if !s.serverImageConversationStorageEnabled() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "server image storage is disabled"})
		return
	}
	userID, ok := identity.UserIDFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthenticated"})
		return
	}
	var body struct {
		Items []imagehistory.Conversation `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}

	store, err := imagehistory.NewStore(s.cfg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer store.Close()

	if err := store.Clear(r.Context(), userID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	for _, item := range body.Items {
		if _, err := store.Save(r.Context(), userID, item); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"imported": len(body.Items)})
}

func (s *Server) serverImageConversationStorageEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(s.cfg.Storage.ImageConversationStorage), "server")
}
