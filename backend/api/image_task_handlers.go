package api

import (
	"encoding/json"
	"net/http"

	"chatgpt2api/internal/identity"
)

// imageTaskUserID resolves the caller's userID from the session context. Every
// task route is gated by requireSession, so a missing userID is an internal
// error rather than an expected anonymous case.
func imageTaskUserID(r *http.Request) (string, bool) {
	return identity.UserIDFromContext(r.Context())
}

func (s *Server) handleCreateImageTask(w http.ResponseWriter, r *http.Request) {
	if s.imageTasks == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "image task manager is unavailable"})
		return
	}
	userID, ok := imageTaskUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthenticated"})
		return
	}
	var body createImageTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	// The owner is taken from the session, never the request body.
	body.UserID = userID
	task, err := s.imageTasks.createTask(userID, body)
	if err != nil {
		writeImageRequestError(w, err)
		return
	}
	_, snapshot := s.imageTasks.listTasks(userID)
	writeJSON(w, http.StatusOK, map[string]any{
		"task":     task,
		"snapshot": snapshot,
	})
}

func (s *Server) handleListImageTasks(w http.ResponseWriter, r *http.Request) {
	if s.imageTasks == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "image task manager is unavailable"})
		return
	}
	userID, ok := imageTaskUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthenticated"})
		return
	}
	items, snapshot := s.imageTasks.listTasks(userID)
	writeJSON(w, http.StatusOK, map[string]any{
		"items":    items,
		"snapshot": snapshot,
	})
}

func (s *Server) handleGetImageTask(w http.ResponseWriter, r *http.Request) {
	if s.imageTasks == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "image task manager is unavailable"})
		return
	}
	userID, ok := imageTaskUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthenticated"})
		return
	}
	task, snapshot, err := s.imageTasks.getTask(userID, r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"task":     task,
		"snapshot": snapshot,
	})
}

func (s *Server) handleCancelImageTask(w http.ResponseWriter, r *http.Request) {
	if s.imageTasks == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "image task manager is unavailable"})
		return
	}
	userID, ok := imageTaskUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthenticated"})
		return
	}
	task, err := s.imageTasks.cancelTask(userID, r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	_, snapshot := s.imageTasks.listTasks(userID)
	writeJSON(w, http.StatusOK, map[string]any{
		"task":     task,
		"snapshot": snapshot,
	})
}

func (s *Server) handleImageTaskSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.imageTasks == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "image task manager is unavailable"})
		return
	}
	userID, ok := imageTaskUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthenticated"})
		return
	}
	_, snapshot := s.imageTasks.listTasks(userID)
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snapshot})
}

func (s *Server) handleImageTaskStream(w http.ResponseWriter, r *http.Request) {
	if s.imageTasks == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "image task manager is unavailable"})
		return
	}
	userID, ok := imageTaskUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthenticated"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	subID, ch := s.imageTasks.subscribe(userID)
	items, snapshot := s.imageTasks.listTasks(userID)
	defer s.imageTasks.unsubscribe(subID)

	initialPayload := map[string]any{
		"items":    items,
		"snapshot": snapshot,
	}
	raw, err := json.Marshal(initialPayload)
	if err == nil {
		_, _ = w.Write([]byte("event: init\n"))
		_, _ = w.Write([]byte("data: " + string(raw) + "\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			if err := writeSSEEvent(w, event); err != nil {
				return
			}
		}
	}
}
