package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"chatgpt2api/internal/config"
)

// --- §8 regression: cross-user image task isolation ---

func newPhase6TaskManager() *imageTaskManager {
	return newImageTaskManager(&Server{cfg: &config.Config{}})
}

// TestPhase6TaskListIsolation: listTasks returns only the caller's own tasks.
func TestPhase6TaskListIsolation(t *testing.T) {
	m := newPhase6TaskManager()
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
	m := newPhase6TaskManager()
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
	m := newPhase6TaskManager()
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
	server := NewServer(cfg, nil, nil)

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
