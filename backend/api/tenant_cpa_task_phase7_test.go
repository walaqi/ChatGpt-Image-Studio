package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"chatgpt2api/internal/credential"
)

// TestPhase7CPATaskUsesPerUserCredential is the §8 item 3 regression guard: an
// async image task in CPA mode must execute against the per-user credential
// resolved from the scheduler-injected userID — never the global [cpa] config
// and never an account-pool lease. It proves the async task path reaches the
// same per-user resolution the sync /v1 path already had.
func TestPhase7CPATaskUsesPerUserCredential(t *testing.T) {
	server, _ := newImageModeCompatTestServerWithOptions(t, imageModeCompatScenario{
		imageMode:        "cpa",
		accountType:      "Free",
		freeRoute:        "legacy",
		freeModel:        "auto",
		paidRoute:        "responses",
		paidModel:        "gpt-5.4-mini",
		cpaRouteStrategy: "images_api",
	}, compatTestServerOptions{})

	const userA = "tenant-A"

	// Per-user credential service: userA's remembered key resolves to a
	// credential distinct from the global [cpa] config (test-cpa-key /
	// 127.0.0.1:8317) so we can prove the per-user one is what gets used.
	resolver := &fakeResolver{
		creds: map[string]credential.Credential{
			credKey(userA, 7): {
				APIKey:  "per-user-A-key",
				BaseURL: "http://per-user-A.local",
				Model:   "gpt-5.4-mini",
			},
		},
	}
	selection := credential.NewMemorySelectionStore()
	if err := selection.Set(context.Background(), userA, 7); err != nil {
		t.Fatalf("seed selection: %v", err)
	}
	server.credService = credential.NewService(resolver, selection)

	// Capture what newCPAWorkflowClient hands the factory. The scheduler runs
	// the unit on a goroutine, so guard the captured values.
	var (
		mu         sync.Mutex
		gotBaseURL string
		gotAPIKey  string
		gotCalls   int
	)
	server.cpaClientFactory = func(baseURL, apiKey, imageModel string, timeout time.Duration, routeStrategy string) cpaRouteAwareImageWorkflowClient {
		mu.Lock()
		gotBaseURL = baseURL
		gotAPIKey = apiKey
		gotCalls++
		mu.Unlock()
		return &compatStubWorkflowClient{
			factory:  "cpa",
			token:    "cpa",
			cpaRoute: routeStrategy,
			model:    imageModel,
		}
	}

	// Drain to a final status before t.TempDir cleanup races the async writer.
	t.Cleanup(func() {
		m := server.imageTasks
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			m.mu.Lock()
			pending := 0
			for _, task := range m.tasks {
				if task != nil && !isFinalImageTaskStatus(task.Status) {
					pending++
				}
			}
			m.mu.Unlock()
			if pending == 0 {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	if _, err := server.imageTasks.createTask(userA, createImageTaskRequest{
		ConversationID: "conv-cpa-1",
		TurnID:         "turn-cpa-1",
		Mode:           "generate",
		Prompt:         "draw a cat",
		Model:          "gpt-image-2",
		Count:          1,
		Size:           "1024x1024",
		Quality:        "high",
	}); err != nil {
		t.Fatalf("createTask: %v", err)
	}

	// Wait for the user-owned task to reach success.
	deadline := time.Now().Add(3 * time.Second)
	var status imageTaskStatus
	for time.Now().Before(deadline) {
		task, _, err := server.imageTasks.getTask(userA, "turn-cpa-1")
		if err == nil && task != nil {
			status = imageTaskStatus(task.Status)
			if status == imageTaskStatusSucceeded {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status != imageTaskStatusSucceeded {
		task, _, _ := server.imageTasks.getTask(userA, "turn-cpa-1")
		errMsg := ""
		if task != nil {
			errMsg = task.Error
		}
		t.Fatalf("task status = %q, want succeeded (error: %s)", status, errMsg)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("cpa factory calls = %d, want 1", gotCalls)
	}
	if gotAPIKey != "per-user-A-key" {
		t.Fatalf("cpa apiKey = %q, want per-user-A-key (per-user credential, not global [cpa] config)", gotAPIKey)
	}
	if gotBaseURL != "http://per-user-A.local" {
		t.Fatalf("cpa baseURL = %q, want http://per-user-A.local (per-user credential)", gotBaseURL)
	}

	task, _, err := server.imageTasks.getTask(userA, "turn-cpa-1")
	if err != nil {
		t.Fatalf("getTask: %v", err)
	}
	if len(task.Images) != 1 {
		t.Fatalf("task images = %d, want 1", len(task.Images))
	}
}
