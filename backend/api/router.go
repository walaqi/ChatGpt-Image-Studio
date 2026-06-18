package api

import (
	"net/http"

	"chatgpt2api/internal/config"
)

func SetupRouter(cfg *config.Config) http.Handler {
	return NewServer(cfg).Handler()
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}
