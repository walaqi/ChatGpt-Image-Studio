package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCreateImageTaskRunsToSuccess(t *testing.T) {
	server, recorder := newCPATestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/image/tasks", strings.NewReader(`{
		"conversationId":"conv-task-1",
		"turnId":"turn-task-1",
		"mode":"generate",
		"prompt":"draw a cat",
		"model":"gpt-image-2",
		"count":1,
		"size":"1248x1248",
		"quality":"high"
	}`))
	req.AddCookie(mintTestSession(t, server, defaultTestUserID))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Task struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"task"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Task.ID != "turn-task-1" {
		t.Fatalf("task id = %q, want turn-task-1", payload.Task.ID)
	}

	waitForTaskStatus(t, server, payload.Task.ID, imageTaskStatusSucceeded)

	task, _, err := server.imageTasks.getTask(defaultTestUserID, payload.Task.ID)
	if err != nil {
		t.Fatalf("getTask() returned error: %v", err)
	}
	if len(task.Images) != 1 {
		t.Fatalf("task images = %d, want 1", len(task.Images))
	}
	if task.Images[0].URL == "" {
		t.Fatalf("task image url = empty, want cached file url")
	}
	if !strings.HasPrefix(task.Images[0].URL, "/v1/files/image/") {
		t.Fatalf("task image url = %q, want relative cached file url", task.Images[0].URL)
	}
	if recorder.calls() != 1 {
		t.Fatalf("cpa calls = %d, want 1", recorder.calls())
	}
}

func TestCreateImageTaskSelectionEditRunsInpaint(t *testing.T) {
	server, recorder := newCPATestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/image/tasks", strings.NewReader(`{
		"conversationId":"conv-selection-1",
		"turnId":"turn-selection-1",
		"mode":"edit",
		"prompt":"selection edit",
		"model":"gpt-image-2",
		"count":1,
		"sourceImages":[
			{
				"id":"mask-1",
				"role":"mask",
				"name":"mask.png",
				"dataUrl":"data:image/png;base64,aW1hZ2U="
			}
		],
		"sourceReference":{
			"original_file_id":"file-1",
			"original_gen_id":"gen-1",
			"conversation_id":"conv-1",
			"parent_message_id":"msg-1",
			"source_account_id":"acct-1"
		}
	}`))
	req.AddCookie(mintTestSession(t, server, defaultTestUserID))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	waitForTaskStatus(t, server, payload.Task.ID, imageTaskStatusSucceeded)
	if len(recorder.callSequence) != 1 {
		t.Fatalf("callSequence = %#v, want 1 entry", recorder.callSequence)
	}
	if !strings.Contains(recorder.callSequence[0], ":selection-edit") {
		t.Fatalf("callSequence[0] = %q, want selection-edit operation", recorder.callSequence[0])
	}
}

func TestImageTaskStreamWritesInitPayload(t *testing.T) {
	server, _ := newCPATestServer(t)

	if _, err := server.imageTasks.createTask(defaultTestUserID, createImageTaskRequest{
		ConversationID: "conv-stream-1",
		TurnID:         "turn-stream-1",
		Mode:           "generate",
		Prompt:         "stream init",
		Model:          "gpt-image-2",
		Count:          1,
		Size:           "1248x1248",
		Quality:        "high",
	}); err != nil {
		t.Fatalf("createTask() returned error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/image/tasks/stream", nil).WithContext(ctx)
	req.AddCookie(mintTestSession(t, server, defaultTestUserID))
	rec := httptest.NewRecorder()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Handler().ServeHTTP(rec, req)
	}()

	time.Sleep(80 * time.Millisecond)
	cancel()
	wg.Wait()

	body := rec.Body.String()
	if !strings.Contains(body, "event: init") {
		t.Fatalf("stream body = %q, want init event", body)
	}
	if !strings.Contains(body, `"turnId":"turn-stream-1"`) {
		t.Fatalf("stream body = %q, want queued task in init payload", body)
	}
	if !strings.Contains(body, `"snapshot"`) {
		t.Fatalf("stream body = %q, want snapshot payload", body)
	}
}

func TestSchedulePublishesQueuedBlockerUpdatesWhenConcurrencyIsFull(t *testing.T) {
	server, _ := newCPATestServer(t)

	task, err := server.imageTasks.newTask(createImageTaskRequest{
		ConversationID: "conv-blocker-1",
		TurnID:         "turn-blocker-1",
		Mode:           "generate",
		Prompt:         "blocked by concurrency",
		Model:          "gpt-image-2",
		Count:          1,
	})
	if err != nil {
		t.Fatalf("newTask() returned error: %v", err)
	}

	server.imageTasks.mu.Lock()
	server.imageTasks.tasks[task.ID] = task
	server.imageTasks.order = append(server.imageTasks.order, task.ID)
	server.imageTasks.runningUnits = server.imageTasks.maxRunningLocked()
	server.imageTasks.mu.Unlock()

	subID, ch := server.imageTasks.subscribe("")
	defer server.imageTasks.unsubscribe(subID)

	if server.imageTasks.tryScheduleOne() {
		t.Fatal("tryScheduleOne() = true, want no work scheduled when concurrency is full")
	}

	timeout := time.After(2 * time.Second)
	for {
		select {
		case <-timeout:
			t.Fatal("timed out waiting for task.upsert blocker update")
		case event := <-ch:
			if event.Type != "task.upsert" || event.Task == nil {
				continue
			}
			if event.Task.ID != task.ID {
				continue
			}
			if event.Task.WaitingReason != imageTaskWaitingReasonGlobalConcurrency {
				t.Fatalf("WaitingReason = %q, want %q", event.Task.WaitingReason, imageTaskWaitingReasonGlobalConcurrency)
			}
			if len(event.Task.Blockers) == 0 || event.Task.Blockers[0].Code != string(imageTaskWaitingReasonGlobalConcurrency) {
				t.Fatalf("Blockers = %#v, want global concurrency blocker", event.Task.Blockers)
			}
			return
		}
	}
}

func TestCancelRunningImageTaskCancelsQueuedUnits(t *testing.T) {
	server, _ := newCPATestServer(t)
	useParallelCPAClient(server, 180*time.Millisecond)

	if _, err := server.imageTasks.createTask(defaultTestUserID, createImageTaskRequest{
		ConversationID: "conv-cancel-1",
		TurnID:         "turn-cancel-1",
		Mode:           "generate",
		Prompt:         "cancel me",
		Model:          "gpt-image-2",
		Count:          3,
	}); err != nil {
		t.Fatalf("createTask() returned error: %v", err)
	}

	waitForTaskPredicate(t, server, "turn-cancel-1", func(task *imageTaskView) bool {
		return task.Status == imageTaskStatusRunning
	})

	if _, err := server.imageTasks.cancelTask(defaultTestUserID, "turn-cancel-1"); err != nil {
		t.Fatalf("cancelTask() returned error: %v", err)
	}

	waitForTaskStatus(t, server, "turn-cancel-1", imageTaskStatusCancelled)

	task, _, err := server.imageTasks.getTask(defaultTestUserID, "turn-cancel-1")
	if err != nil {
		t.Fatalf("getTask() returned error: %v", err)
	}
	cancelledUnits := 0
	for _, image := range task.Images {
		if image.Error == "任务已取消" {
			cancelledUnits++
		}
	}
	if cancelledUnits < 1 {
		t.Fatalf("task images = %#v, want at least one queued unit cancelled", task.Images)
	}
}

func TestCancelRunningImageTaskInterruptsUpstreamRequest(t *testing.T) {
	server, _ := newCPATestServer(t)
	useParallelCPAClient(server, 5*time.Second)

	if _, err := server.imageTasks.createTask(defaultTestUserID, createImageTaskRequest{
		ConversationID: "conv-cancel-fast-1",
		TurnID:         "turn-cancel-fast-1",
		Mode:           "generate",
		Prompt:         "cancel fast",
		Model:          "gpt-image-2",
		Count:          1,
	}); err != nil {
		t.Fatalf("createTask() returned error: %v", err)
	}

	waitForTaskPredicate(t, server, "turn-cancel-fast-1", func(task *imageTaskView) bool {
		return task.Status == imageTaskStatusRunning
	})

	startedAt := time.Now()
	if _, err := server.imageTasks.cancelTask(defaultTestUserID, "turn-cancel-fast-1"); err != nil {
		t.Fatalf("cancelTask() returned error: %v", err)
	}

	waitForTaskStatus(t, server, "turn-cancel-fast-1", imageTaskStatusCancelled)

	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("cancel finished in %s, want running request to stop promptly", elapsed)
	}
}

func TestDeferredQueuedUnitSchedulesRetryWakeup(t *testing.T) {
	server, _ := newCPATestServer(t)

	task, err := server.imageTasks.newTask(createImageTaskRequest{
		ConversationID: "conv-backoff-1",
		TurnID:         "turn-backoff-1",
		Mode:           "generate",
		Prompt:         "backoff",
		Model:          "gpt-image-2",
		Count:          1,
	})
	if err != nil {
		t.Fatalf("newTask() returned error: %v", err)
	}

	server.imageTasks.mu.Lock()
	task.Units[0].NextAttemptAt = time.Now().Add(3 * time.Second)
	server.imageTasks.tasks[task.ID] = task
	server.imageTasks.order = append(server.imageTasks.order, task.ID)
	server.imageTasks.mu.Unlock()

	if server.imageTasks.tryScheduleOne() {
		t.Fatal("tryScheduleOne() = true, want deferred task to stay queued")
	}

	server.imageTasks.mu.Lock()
	defer server.imageTasks.mu.Unlock()
	if server.imageTasks.scheduleAt.IsZero() {
		t.Fatal("scheduleAt = zero, want retry wakeup to be scheduled")
	}
	if !server.imageTasks.scheduleAt.After(time.Now()) {
		t.Fatalf("scheduleAt = %s, want future retry wakeup", server.imageTasks.scheduleAt)
	}
}

func TestQueuedImageTaskExpiresBeforeFirstRun(t *testing.T) {
	server, _ := newCPATestServer(t)
	server.cfg.Server.MaxImageConcurrency = 1
	server.cfg.Server.ImageTaskQueueTTLSeconds = 1
	useParallelCPAClient(server, 1500*time.Millisecond)

	if _, err := server.imageTasks.createTask(defaultTestUserID, createImageTaskRequest{
		ConversationID: "conv-expire-runner",
		TurnID:         "turn-expire-runner",
		Mode:           "generate",
		Prompt:         "occupy slot",
		Model:          "gpt-image-2",
		Count:          1,
	}); err != nil {
		t.Fatalf("createTask(runner) returned error: %v", err)
	}
	waitForTaskPredicate(t, server, "turn-expire-runner", func(task *imageTaskView) bool {
		return task.Status == imageTaskStatusRunning
	})

	if _, err := server.imageTasks.createTask(defaultTestUserID, createImageTaskRequest{
		ConversationID: "conv-expire-queued",
		TurnID:         "turn-expire-queued",
		Mode:           "generate",
		Prompt:         "should expire",
		Model:          "gpt-image-2",
		Count:          1,
	}); err != nil {
		t.Fatalf("createTask(queued) returned error: %v", err)
	}

	waitForTaskStatus(t, server, "turn-expire-queued", imageTaskStatusExpired)

	task, _, err := server.imageTasks.getTask(defaultTestUserID, "turn-expire-queued")
	if err != nil {
		t.Fatalf("getTask() returned error: %v", err)
	}
	if task.Error == "" {
		t.Fatal("expired task error = empty, want timeout message")
	}
	if len(task.Images) != 1 || task.Images[0].Error == "" {
		t.Fatalf("task images = %#v, want queued image marked as error", task.Images)
	}
}

func TestCompletedImageTaskIsPrunedAfterRetention(t *testing.T) {
	server, _ := newCPATestServer(t)

	task, err := server.imageTasks.newTask(createImageTaskRequest{
		ConversationID: "conv-prune-1",
		TurnID:         "turn-prune-1",
		Mode:           "generate",
		Prompt:         "prune me",
		Model:          "gpt-image-2",
		Count:          1,
	})
	if err != nil {
		t.Fatalf("newTask() returned error: %v", err)
	}

	server.imageTasks.mu.Lock()
	task.Status = imageTaskStatusSucceeded
	task.FinishedAt = time.Now().Add(-imageTaskRetentionAfterFinish - time.Minute)
	server.imageTasks.tasks[task.ID] = task
	server.imageTasks.order = append(server.imageTasks.order, task.ID)
	server.imageTasks.mu.Unlock()

	if !server.imageTasks.tryScheduleOne() {
		t.Fatal("tryScheduleOne() = false, want prune cycle to run")
	}

	if _, _, err := server.imageTasks.getTask(defaultTestUserID, task.ID); err == nil {
		t.Fatalf("getTask(%s) error = nil, want pruned task to be removed", task.ID)
	}
	items, snapshot := server.imageTasks.listTasks("")
	if len(items) != 0 {
		t.Fatalf("len(items) = %d, want 0 after pruning", len(items))
	}
	if snapshot.Total != 0 {
		t.Fatalf("snapshot.Total = %d, want 0 after pruning", snapshot.Total)
	}
}

func TestTaskSnapshotCountsQueuedUnitsWithinRunningParentTask(t *testing.T) {
	server, _ := newCPATestServer(t)

	task, err := server.imageTasks.newTask(createImageTaskRequest{
		ConversationID: "conv-parent-1",
		TurnID:         "turn-parent-1",
		Mode:           "generate",
		Prompt:         "multi image",
		Model:          "gpt-image-2",
		Count:          6,
	})
	if err != nil {
		t.Fatalf("newTask() returned error: %v", err)
	}

	server.imageTasks.mu.Lock()
	task.Status = imageTaskStatusRunning
	task.ActiveUnits = 2
	task.Units[0].Status = imageTaskStatusRunning
	task.Units[1].Status = imageTaskStatusRunning
	server.imageTasks.tasks[task.ID] = task
	server.imageTasks.order = append(server.imageTasks.order, task.ID)
	server.imageTasks.runningUnits = 2
	snapshot := server.imageTasks.snapshotLocked("")
	server.imageTasks.mu.Unlock()

	if snapshot.Running != 2 {
		t.Fatalf("snapshot.Running = %d, want 2", snapshot.Running)
	}
	if snapshot.Queued != 4 {
		t.Fatalf("snapshot.Queued = %d, want 4 queued units", snapshot.Queued)
	}
	if snapshot.ActiveSources.Workspace != 6 {
		t.Fatalf("snapshot.ActiveSources.Workspace = %d, want 6 active units", snapshot.ActiveSources.Workspace)
	}
}

func TestTaskQueuePositionCountsQueuedUnits(t *testing.T) {
	server, _ := newCPATestServer(t)

	firstTask, err := server.imageTasks.newTask(createImageTaskRequest{
		ConversationID: "conv-queue-a",
		TurnID:         "turn-queue-a",
		Mode:           "generate",
		Prompt:         "first queued parent",
		Model:          "gpt-image-2",
		Count:          3,
	})
	if err != nil {
		t.Fatalf("newTask(first) returned error: %v", err)
	}
	secondTask, err := server.imageTasks.newTask(createImageTaskRequest{
		ConversationID: "conv-queue-b",
		TurnID:         "turn-queue-b",
		Mode:           "generate",
		Prompt:         "second queued parent",
		Model:          "gpt-image-2",
		Count:          2,
	})
	if err != nil {
		t.Fatalf("newTask(second) returned error: %v", err)
	}

	server.imageTasks.mu.Lock()
	server.imageTasks.tasks[firstTask.ID] = firstTask
	server.imageTasks.tasks[secondTask.ID] = secondTask
	server.imageTasks.order = append(server.imageTasks.order, firstTask.ID, secondTask.ID)
	view := server.imageTasks.buildTaskViewLocked(secondTask)
	server.imageTasks.mu.Unlock()

	if view.QueuePosition != 4 {
		t.Fatalf("QueuePosition = %d, want 4 because 3 queued units are ahead", view.QueuePosition)
	}
}
