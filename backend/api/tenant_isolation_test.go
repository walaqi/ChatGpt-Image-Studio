package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chatgpt2api/internal/config"
	"chatgpt2api/internal/imagehistory"
)

// newTenantTestServer builds a server backed by server-mode sqlite history with
// a session manager, plus a helper to mint a per-user session cookie.
func newTenantTestServer(t *testing.T) (*Server, func(userID string) *http.Cookie) {
	t.Helper()
	cfg := config.New(t.TempDir())
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Storage.Backend = "sqlite"
	cfg.Storage.SQLitePath = "data/history.sqlite"
	cfg.Storage.ImageConversationStorage = "server"
	cfg.Storage.ImageDataStorage = "server"
	cfg.Identity.SessionSecret = "tenant-test-secret"
	cfg.Identity.SessionTTLSeconds = 3600

	server := NewServer(cfg, nil, nil)
	mint := func(userID string) *http.Cookie {
		token, err := server.sessionManager.Mint(userID)
		if err != nil {
			t.Fatalf("Mint(%s): %v", userID, err)
		}
		return &http.Cookie{Name: sessionCookieName, Value: token}
	}
	return server, mint
}

// TestHistoryIsolationBetweenUsers proves userA cannot see userB's saved
// conversations via the HTTP API.
func TestHistoryIsolationBetweenUsers(t *testing.T) {
	server, mint := newTenantTestServer(t)
	handler := server.Handler()

	// userA saves a conversation directly through the store (server-mode).
	store, err := imagehistory.NewStore(server.cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()
	if _, err := store.Save(context.Background(), "userA", imagehistory.Conversation{
		ID:        "conv-a",
		Title:     "A's image",
		CreatedAt: "2026-06-15T00:00:00Z",
		Status:    "success",
		Turns: []imagehistory.Turn{{
			ID: "t1", CreatedAt: "2026-06-15T00:00:00Z", Status: "success",
		}},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// userB lists conversations → must NOT see conv-a.
	req := httptest.NewRequest(http.MethodGet, "/api/image/conversations", nil)
	req.AddCookie(mint("userB"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "conv-a") {
		t.Fatalf("userB saw userA's conversation: %s", body)
	}

	// userA lists → must see conv-a.
	reqA := httptest.NewRequest(http.MethodGet, "/api/image/conversations", nil)
	reqA.AddCookie(mint("userA"))
	recA := httptest.NewRecorder()
	handler.ServeHTTP(recA, reqA)
	if !strings.Contains(recA.Body.String(), "conv-a") {
		t.Fatalf("userA could not see own conversation: %s", recA.Body.String())
	}
}

// TestImageFileOwnershipEnforced proves userB cannot download a file stored
// under userA's namespace.
func TestImageFileOwnershipEnforced(t *testing.T) {
	server, mint := newTenantTestServer(t)
	handler := server.Handler()

	// Place a file under userA's image subdirectory.
	userADir := filepath.Join(server.cfg.ResolvePath(server.cfg.Storage.ImageDir), "userA")
	if err := os.MkdirAll(userADir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userADir, "result-secret.png"), []byte("secret"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// userB requests userA's file → must be denied (403).
	req := httptest.NewRequest(http.MethodGet, "/v1/files/image/userA/result-secret.png", nil)
	req.AddCookie(mint("userB"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("userB access to userA file = %d, want 403", rec.Code)
	}

	// userA requests own file → must succeed.
	reqA := httptest.NewRequest(http.MethodGet, "/v1/files/image/userA/result-secret.png", nil)
	reqA.AddCookie(mint("userA"))
	recA := httptest.NewRecorder()
	handler.ServeHTTP(recA, reqA)
	if recA.Code != http.StatusOK {
		t.Fatalf("userA access to own file = %d, want 200", recA.Code)
	}
	if recA.Body.String() != "secret" {
		t.Fatalf("served body = %q, want secret", recA.Body.String())
	}
}

// TestGatewayImageURLPrefix verifies the public base path is prepended to the
// absolute image URL so it routes back through the /image-studio reverse proxy.
func TestGatewayImageURLPrefix(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/images/generations", nil)
	req.Host = "app.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")

	got := gatewayImageURL(req, "/image-studio", "userA/result-x.png")
	want := "https://app.example.com/image-studio/v1/files/image/userA/result-x.png"
	if got != want {
		t.Fatalf("gatewayImageURL = %q, want %q", got, want)
	}

	// Empty base path (local dev) yields no prefix.
	got2 := gatewayImageURL(req, "", "userA/result-x.png")
	want2 := "https://app.example.com/v1/files/image/userA/result-x.png"
	if got2 != want2 {
		t.Fatalf("gatewayImageURL (no prefix) = %q, want %q", got2, want2)
	}
}
