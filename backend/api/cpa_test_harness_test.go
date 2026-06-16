package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"chatgpt2api/handler"
	"chatgpt2api/internal/config"
	"chatgpt2api/internal/credential"
)

// defaultTestUserID is the canonical tenant used by harness-built tests. Phase 7
// removed the legacy single-tenant bearer, so tests mint a session for this user
// (mintTestSession) or call the task manager directly with this ID.
const defaultTestUserID = "default"

// --- cpa-only test harness ---
//
// Phase 7 deleted the studio account pool AND the single-tenant fallback, so
// the old account-seeding harness (newImageModeCompatTestServerWithOptions) is
// gone and every request must carry a session-derived userID with a resolvable
// per-user credential. These helpers build a multi-tenant CPA server wired to a
// permissive in-memory credService (every userID resolves to a stub credential)
// plus a stub CPA workflow client, which is all the surviving task / compat
// lifecycle tests need.

// mintTestSession mints a studio_sid session cookie for userID so HTTP tests
// authenticate the same way the browser does (no legacy bearer post-phase-7).
func mintTestSession(t *testing.T, server *Server, userID string) *http.Cookie {
	t.Helper()
	token, err := server.sessionManager.Mint(userID)
	if err != nil {
		t.Fatalf("mint session: %v", err)
	}
	return &http.Cookie{Name: sessionCookieName, Value: token}
}

// cpaCallRecorder captures stub CPA client invocations for assertions.
type cpaCallRecorder struct {
	mu           sync.Mutex
	cpaCalls     int
	callSequence []string
	lastRoute    string
}

func (r *cpaCallRecorder) record(operation, route string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cpaCalls++
	r.lastRoute = route
	r.callSequence = append(r.callSequence, "cpa:"+operation)
}

func (r *cpaCallRecorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cpaCalls
}

// cpaStubWorkflowClient is a route-aware stub CPA image client.
type cpaStubWorkflowClient struct {
	route       string
	model       string
	recorder    *cpaCallRecorder
	generateErr error
	editErr     error
	inpaintErr  error
}

func (c *cpaStubWorkflowClient) DownloadBytes(url string) ([]byte, error) {
	return []byte("stub:" + url), nil
}

func (c *cpaStubWorkflowClient) DownloadAsBase64(ctx context.Context, url string) (string, error) {
	_ = ctx
	return base64.StdEncoding.EncodeToString([]byte("stub-image:" + url)), nil
}

func (c *cpaStubWorkflowClient) GenerateImage(ctx context.Context, prompt, model string, n int, size, quality, background string) ([]handler.ImageResult, error) {
	_ = ctx
	_ = prompt
	_ = n
	_ = size
	_ = quality
	_ = background
	if c.recorder != nil {
		c.recorder.record("generate", c.route)
	}
	if c.generateErr != nil {
		return nil, c.generateErr
	}
	return []handler.ImageResult{{URL: "stub://generated", RevisedPrompt: "stub"}}, nil
}

func (c *cpaStubWorkflowClient) EditImageByUpload(ctx context.Context, prompt, model string, images [][]byte, mask []byte, size, quality string) ([]handler.ImageResult, error) {
	_ = ctx
	_ = prompt
	_ = images
	_ = mask
	_ = size
	_ = quality
	if c.recorder != nil {
		c.recorder.record("edit", c.route)
	}
	if c.editErr != nil {
		return nil, c.editErr
	}
	return []handler.ImageResult{{URL: "stub://edited", RevisedPrompt: "stub"}}, nil
}

func (c *cpaStubWorkflowClient) InpaintImageByMask(ctx context.Context, prompt string, model string, originalFileID string, originalGenID string, conversationID string, parentMessageID string, mask []byte, size string, quality string) ([]handler.ImageResult, error) {
	_ = ctx
	_ = prompt
	_ = originalFileID
	_ = originalGenID
	_ = conversationID
	_ = parentMessageID
	_ = mask
	_ = size
	_ = quality
	if c.recorder != nil {
		c.recorder.record("selection-edit", c.route)
	}
	if c.inpaintErr != nil {
		return nil, c.inpaintErr
	}
	return []handler.ImageResult{{URL: "stub://inpaint", RevisedPrompt: "stub"}}, nil
}

func (c *cpaStubWorkflowClient) LastRoute() string      { return c.route }
func (c *cpaStubWorkflowClient) LastModelLabel() string { return c.model }

// alwaysResolver is a permissive credential.Resolver for the cpa harness: every
// (userID, keyID) resolves to the same stub-backed credential. Phase 7 removed
// the global-[cpa]-config fallback, so a credService is now mandatory even for
// single-tenant tests; this stands in for the mother system.
type alwaysResolver struct {
	cred credential.Credential
}

func (r *alwaysResolver) ListKeys(_ context.Context, _ string) (credential.KeyListResult, error) {
	return credential.KeyListResult{
		Keys:      []credential.KeyCandidate{{KeyID: 1, Name: "stub"}},
		CanCreate: true,
	}, nil
}

func (r *alwaysResolver) Resolve(_ context.Context, _ string, _ int64) (credential.Credential, error) {
	return r.cred, nil
}

// newCPATestServer builds a CPA-mode server with a stub CPA client and a call
// recorder. A permissive credService resolves every user to the stub credential
// and the default tenant's key selection is pre-seeded, so direct createTask
// calls succeed. HTTP tests authenticate with mintTestSession.
func newCPATestServer(t *testing.T) (*Server, *cpaCallRecorder) {
	t.Helper()

	cfg := config.New(t.TempDir())
	if err := cfg.Load(); err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.App.ImageFormat = "b64_json"
	cfg.ChatGPT.Model = "gpt-image-2"
	cfg.ChatGPT.ImageMode = "cpa"
	cfg.CPA.BaseURL = "http://127.0.0.1:8317"
	cfg.CPA.APIKey = "test-cpa-key"
	cfg.CPA.RequestTimeout = 60
	cfg.CPA.RouteStrategy = "images_api"
	cfg.Identity.SessionSecret = "test-session-secret"
	cfg.Identity.SessionTTLSeconds = 3600

	server := NewServer(cfg)

	resolver := &alwaysResolver{cred: credential.Credential{
		BaseURL: cfg.CPA.BaseURL,
		APIKey:  cfg.CPA.APIKey,
	}}
	selection := credential.NewMemorySelectionStore()
	if err := selection.Set(context.Background(), defaultTestUserID, 1); err != nil {
		t.Fatalf("seed selection: %v", err)
	}
	server.credService = credential.NewService(resolver, selection)

	recorder := &cpaCallRecorder{}
	server.cpaClientFactory = func(baseURL, apiKey, imageModel string, timeout time.Duration, routeStrategy string) cpaRouteAwareImageWorkflowClient {
		_ = baseURL
		_ = apiKey
		_ = timeout
		return &cpaStubWorkflowClient{
			route:    firstNonEmpty(routeStrategy, "images_api"),
			model:    imageModel,
			recorder: recorder,
		}
	}

	// Async execution goroutines keep writing into the temp data dir; drain
	// every task to a final status before t.TempDir cleanup runs RemoveAll,
	// otherwise cleanup races the writers ("directory not empty").
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

	return server, recorder
}

func waitForTaskStatus(t *testing.T, server *Server, taskID string, want imageTaskStatus) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		task, _, err := server.imageTasks.getTask(defaultTestUserID, taskID)
		if err == nil && task != nil && imageTaskStatus(task.Status) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	task, _, err := server.imageTasks.getTask(defaultTestUserID, taskID)
	if err != nil {
		t.Fatalf("getTask(%s) returned error: %v", taskID, err)
	}
	t.Fatalf("task %s status = %q, want %q", taskID, task.Status, want)
}

func waitForTaskPredicate(t *testing.T, server *Server, taskID string, predicate func(*imageTaskView) bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		task, _, err := server.imageTasks.getTask(defaultTestUserID, taskID)
		if err == nil && task != nil && predicate(task) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	task, _, err := server.imageTasks.getTask(defaultTestUserID, taskID)
	if err != nil {
		t.Fatalf("getTask(%s) returned error: %v", taskID, err)
	}
	t.Fatalf("task %s did not satisfy predicate, current status = %q", taskID, task.Status)
}

// parallelGenerateWorkflowClient is a CPA stub that tracks concurrent calls and
// honors context cancellation, for cancel/concurrency tests.
type parallelGenerateWorkflowClient struct {
	token     string
	active    *int32
	maxActive *int32
	delay     time.Duration
}

func (c *parallelGenerateWorkflowClient) DownloadBytes(url string) ([]byte, error) {
	return []byte("parallel:" + url), nil
}

func (c *parallelGenerateWorkflowClient) DownloadAsBase64(ctx context.Context, url string) (string, error) {
	_ = ctx
	return base64.StdEncoding.EncodeToString([]byte("parallel:" + url)), nil
}

func (c *parallelGenerateWorkflowClient) GenerateImage(ctx context.Context, prompt, model string, n int, size, quality, background string) ([]handler.ImageResult, error) {
	_ = prompt
	_ = model
	_ = n
	_ = size
	_ = quality
	_ = background

	active := atomic.AddInt32(c.active, 1)
	for {
		maxActive := atomic.LoadInt32(c.maxActive)
		if active <= maxActive || atomic.CompareAndSwapInt32(c.maxActive, maxActive, active) {
			break
		}
	}
	defer atomic.AddInt32(c.active, -1)
	if c.delay > 0 {
		timer := time.NewTimer(c.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}

	return []handler.ImageResult{{URL: "stub://parallel/" + c.token, RevisedPrompt: "parallel"}}, nil
}

func (c *parallelGenerateWorkflowClient) EditImageByUpload(ctx context.Context, prompt, model string, images [][]byte, mask []byte, size, quality string) ([]handler.ImageResult, error) {
	_ = ctx
	_ = prompt
	_ = model
	_ = images
	_ = mask
	_ = size
	_ = quality
	return nil, fmt.Errorf("not implemented")
}

func (c *parallelGenerateWorkflowClient) InpaintImageByMask(ctx context.Context, prompt string, model string, originalFileID string, originalGenID string, conversationID string, parentMessageID string, mask []byte, size string, quality string) ([]handler.ImageResult, error) {
	_ = ctx
	_ = prompt
	_ = model
	_ = originalFileID
	_ = originalGenID
	_ = conversationID
	_ = parentMessageID
	_ = mask
	_ = size
	_ = quality
	return nil, fmt.Errorf("not implemented")
}

func (c *parallelGenerateWorkflowClient) LastRoute() string      { return "images_api" }
func (c *parallelGenerateWorkflowClient) LastModelLabel() string { return "" }

// useParallelCPAClient swaps in a parallelGenerateWorkflowClient as the server's
// CPA client factory, returning shared active/maxActive counters.
func useParallelCPAClient(server *Server, delay time.Duration) (active, maxActive *int32) {
	active = new(int32)
	maxActive = new(int32)
	server.cpaClientFactory = func(baseURL, apiKey, imageModel string, timeout time.Duration, routeStrategy string) cpaRouteAwareImageWorkflowClient {
		_ = baseURL
		_ = apiKey
		_ = imageModel
		_ = timeout
		_ = routeStrategy
		return &parallelGenerateWorkflowClient{
			token:     "cpa",
			active:    active,
			maxActive: maxActive,
			delay:     delay,
		}
	}
	return active, maxActive
}
