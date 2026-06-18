package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"chatgpt2api/internal/config"
)

// --- §8 regression: cross-user image task isolation ---

// newPhase6TaskManager builds a cpa-mode server (per-user credentials) and
// returns its task manager. The harness drains async tasks on cleanup.
func newPhase6TaskManager(t *testing.T) *imageTaskManager {
	t.Helper()
	server, _ := newCPATestServer(t)
	return server.imageTasks
}

// TestPhase6TaskListIsolation: listTasks returns only the caller's own tasks.
func TestPhase6TaskListIsolation(t *testing.T) {
	m := newPhase6TaskManager(t)
	if _, err := m.createTask("userA", createImageTaskRequest{
		TurnID: "a-1", Mode: "generate", Prompt: "a", Count: 1,
	}); err != nil {
		t.Fatalf("createTask userA: %v", err)
	}
	if _, err := m.createTask("userB", createImageTaskRequest{
		TurnID: "b-1", Mode: "generate", Prompt: "b", Count: 1,
	}); err != nil {
		t.Fatalf("createTask userB: %v", err)
	}

	aTasks, _ := m.listTasks("userA")
	if len(aTasks) != 1 || aTasks[0].ID != "a-1" {
		t.Fatalf("userA sees %d tasks, want only a-1", len(aTasks))
	}
	bTasks, _ := m.listTasks("userB")
	if len(bTasks) != 1 || bTasks[0].ID != "b-1" {
		t.Fatalf("userB sees %d tasks, want only b-1", len(bTasks))
	}
}

// TestPhase6TaskGetCancelOwnership: get/cancel refuse cross-tenant access.
func TestPhase6TaskGetCancelOwnership(t *testing.T) {
	m := newPhase6TaskManager(t)
	if _, err := m.createTask("userA", createImageTaskRequest{
		TurnID: "a-1", Mode: "generate", Prompt: "a", Count: 1,
	}); err != nil {
		t.Fatalf("createTask: %v", err)
	}
	if _, _, err := m.getTask("userB", "a-1"); err == nil {
		t.Fatal("userB getTask(a-1) should fail")
	}
	if _, err := m.cancelTask("userB", "a-1"); err == nil {
		t.Fatal("userB cancelTask(a-1) should fail")
	}
	if _, _, err := m.getTask("userA", "a-1"); err != nil {
		t.Fatalf("userA getTask(a-1) should succeed: %v", err)
	}
}

// TestPhase6TaskStreamIsolation: a subscriber only receives its own events.
func TestPhase6TaskStreamIsolation(t *testing.T) {
	m := newPhase6TaskManager(t)
	subID, ch := m.subscribe("userA")
	defer m.unsubscribe(subID)

	if _, err := m.createTask("userB", createImageTaskRequest{
		TurnID: "b-1", Mode: "generate", Prompt: "b", Count: 1,
	}); err != nil {
		t.Fatalf("createTask userB: %v", err)
	}
	select {
	case ev := <-ch:
		t.Fatalf("userA received userB event: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}

	if _, err := m.createTask("userA", createImageTaskRequest{
		TurnID: "a-1", Mode: "generate", Prompt: "a", Count: 1,
	}); err != nil {
		t.Fatalf("createTask userA: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.Task == nil || ev.Task.ID != "a-1" {
			t.Fatalf("userA expected a-1, got %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("userA did not receive own event")
	}
}

// --- §8 regression: source-image cross-tenant reuse guard ---

func TestPhase6SourceImageReuseCrossTenantDenied(t *testing.T) {
	cfg := config.New(t.TempDir())
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	server := NewServer(cfg)

	// A file under userB's namespace.
	userBDir := filepath.Join(server.cfg.ResolvePath(server.cfg.Storage.ImageDir), "userB")
	if err := os.MkdirAll(userBDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userBDir, "result-x.png"), []byte("secret"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// userA must not reuse userB's image by crafting a URL.
	if _, err := server.resolveTaskSourceImageBytes("userA", imageTaskSourceImage{
		URL: "/v1/files/image/userB/result-x.png",
	}); err == nil {
		t.Fatal("userA reusing userB's source image should be denied")
	}

	// userB reuses its own image.
	data, err := server.resolveTaskSourceImageBytes("userB", imageTaskSourceImage{
		URL: "/v1/files/image/userB/result-x.png",
	})
	if err != nil {
		t.Fatalf("userB reusing own image: %v", err)
	}
	if string(data) != "secret" {
		t.Fatalf("got %q, want secret", string(data))
	}
}

// --- §8 regression: import endpoint rejects storage-backend coordinates ---

// newImportTestServer builds a server in server-storage mode with a file
// backend rooted in a temp dir. Requests authenticate with a session cookie
// (minted for defaultTestUserID). It returns the server, its handler, an
// auth cookie, and the temp root so the test can assert where data landed.
func newImportTestServer(t *testing.T) (*Server, http.Handler, *http.Cookie, string) {
	t.Helper()
	root := t.TempDir()
	cfg := config.New(root)
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Identity.SessionSecret = "test-session-secret"
	cfg.Identity.SessionTTLSeconds = 3600
	cfg.Storage.Backend = "current"
	cfg.Storage.ImageDir = "data/images"
	cfg.Storage.ImageConversationStorage = "server"
	cfg.Storage.ImageDataStorage = "server"
	cfg.Storage.ImageStorage = "server"
	server := NewServer(cfg)
	return server, server.Handler(), mintTestSession(t, server, defaultTestUserID), root
}

// TestPhase6ImportIgnoresStorageCoordinates proves the import endpoint accepts
// ONLY conversation items: any backend/redisAddr/sqlitePath/imageDir coordinates
// smuggled in the request body are ignored (the handler decodes them into
// nothing), so a tenant cannot repoint the server at an arbitrary Redis or path.
// Data must land in the server's own configured store under the caller's userID,
// never in the attacker-supplied location.
func TestPhase6ImportIgnoresStorageCoordinates(t *testing.T) {
	_, handler, authCookie, root := newImportTestServer(t)

	// A directory the attacker tries to redirect storage into. If the handler
	// honored coordinates, conversation files would appear here.
	evilDir := filepath.Join(t.TempDir(), "evil-redirect")

	body := map[string]any{
		"items": []map[string]any{
			{
				"id":        "imported-conv",
				"title":     "生成",
				"mode":      "generate",
				"prompt":    "hi",
				"model":     "gpt-image-2",
				"count":     1,
				"createdAt": "2026-04-26T00:00:00Z",
				"status":    "success",
			},
		},
		// Storage coordinates that MUST be ignored.
		"storage": map[string]any{
			"backend":     "redis",
			"redisAddr":   "10.0.0.1:6379",
			"redisPrefix": "attacker",
			"sqlitePath":  filepath.Join(evilDir, "evil.sqlite"),
			"imageDir":    evilDir,
		},
		"backend":    "redis",
		"redisAddr":  "10.0.0.1:6379",
		"sqlitePath": filepath.Join(evilDir, "evil.sqlite"),
		"imageDir":   evilDir,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/image/conversations/import", bytes.NewReader(raw))
	req.AddCookie(authCookie)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	// The attacker-supplied directory must not exist: no storage coordinate was honored.
	if _, err := os.Stat(evilDir); !os.IsNotExist(err) {
		t.Fatalf("attacker storage dir should never be created, err=%v", err)
	}

	// Data must be readable from the server's OWN configured store via the list
	// endpoint, scoped to the caller's userID (legacy bearer → defaultTestUserID).
	listReq := httptest.NewRequest(http.MethodGet, "/api/image/conversations", nil)
	listReq.AddCookie(authCookie)
	listRec := httptest.NewRecorder()
	handler.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200 (body: %s)", listRec.Code, listRec.Body.String())
	}
	var listBody struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listBody); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listBody.Items) != 1 || listBody.Items[0].ID != "imported-conv" {
		t.Fatalf("expected imported-conv in server store, got %#v", listBody.Items)
	}

	// The file landed under the server's configured imageDir, not the evil dir.
	if _, err := os.Stat(filepath.Join(root, "data", "images")); err != nil {
		t.Fatalf("server image dir should exist: %v", err)
	}
}
