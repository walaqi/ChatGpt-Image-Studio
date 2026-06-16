package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"chatgpt2api/internal/identity"
	"chatgpt2api/internal/imagehistory"
)

const maxImageTaskParallelUnitsPerTask = 2
const maxImageTaskDeferredAttempts = 5
const imageTaskRetentionAfterFinish = 30 * time.Minute

var (
	imageTaskRetryBackoffBase = 2 * time.Second
	imageTaskRetryBackoffMax  = 20 * time.Second
)

// imageTaskLease marks a scheduled task unit. In the multi-tenant CPA model
// there is no account-pool lease to hold; execution resolves a per-user
// credential from the task's userID (see executeImageTaskUnit). Concurrency is
// bounded by the task manager's runningUnits cap. The struct is retained as a
// scheduler handle so the run/release plumbing stays uniform.
type imageTaskLease struct {
	release func()
}

// imageTaskSubscriber is one SSE listener, scoped to the user that opened the
// stream. Events are only delivered to subscribers whose userID matches the
// event's owner, so one tenant never sees another's task activity.
type imageTaskSubscriber struct {
	ch     chan imageTaskEvent
	userID string
}

type imageTaskManager struct {
	server        *Server
	mu            sync.Mutex
	scheduleMu    sync.Mutex
	scheduleTimer *time.Timer
	scheduleAt    time.Time
	tasks         map[string]*imageTask
	order         []string
	runningUnits  int
	subscribers   map[int]*imageTaskSubscriber
	nextSubID     int
}

func newImageTaskManager(server *Server) *imageTaskManager {
	return &imageTaskManager{
		server:      server,
		tasks:       map[string]*imageTask{},
		subscribers: map[int]*imageTaskSubscriber{},
	}
}

func (m *imageTaskManager) createTask(userID string, req createImageTaskRequest) (*imageTaskView, error) {
	req.UserID = userID
	task, err := m.newTask(req)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if existing := m.tasks[task.ID]; existing != nil && !isFinalImageTaskStatus(existing.Status) {
		m.mu.Unlock()
		return nil, newRequestError("image_task_already_active", "当前图片任务仍在处理中，请稍后再试")
	}
	m.removeTaskIDFromOrderLocked(task.ID)
	m.tasks[task.ID] = task
	m.order = append(m.order, task.ID)
	view := m.buildTaskViewLocked(task)
	snapshot := m.snapshotLocked(task.UserID)
	subscribers := m.subscriberChannelsLocked()
	m.mu.Unlock()

	m.broadcast(subscribers, imageTaskEvent{
		Type:     "task.upsert",
		Task:     view,
		Snapshot: snapshot,
	})
	if expiresAt := m.initialQueueExpiryAt(task); !expiresAt.IsZero() {
		m.scheduleAfter(expiresAt)
	}
	m.triggerSchedule()
	return view, nil
}

func (m *imageTaskManager) listTasks(userID string) ([]imageTaskView, *imageTaskSnapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()

	items := make([]imageTaskView, 0, len(m.order))
	for _, id := range m.order {
		task := m.tasks[id]
		if task == nil {
			continue
		}
		if task.UserID != userID {
			continue
		}
		items = append(items, *m.buildTaskViewLocked(task))
	}
	snapshot := m.snapshotLocked(userID)
	return items, snapshot
}

// getTask returns the task view + snapshot for the owning user. A task owned by
// another user is reported as not found so one tenant can never probe another's
// task existence.
func (m *imageTaskManager) getTask(userID, id string) (*imageTaskView, *imageTaskSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	task := m.tasks[strings.TrimSpace(id)]
	if task == nil || task.UserID != userID {
		return nil, nil, fmt.Errorf("task not found")
	}
	return m.buildTaskViewLocked(task), m.snapshotLocked(userID), nil
}

func (m *imageTaskManager) waitForTask(ctx context.Context, userID, taskID string) (*imageTaskView, error) {
	if taskID = strings.TrimSpace(taskID); taskID == "" {
		return nil, fmt.Errorf("task id is required")
	}
	if task, _, err := m.getTask(userID, taskID); err == nil && isFinalImageTaskStatus(task.Status) {
		return task, nil
	}

	subID, ch := m.subscribe(userID)
	defer m.unsubscribe(subID)

	for {
		select {
		case <-ctx.Done():
			task, _, err := m.getTask(userID, taskID)
			if err == nil && task != nil && task.Status == imageTaskStatusQueued {
				_, _ = m.cancelTask(userID, taskID)
			}
			return nil, ctx.Err()
		case event, ok := <-ch:
			if !ok {
				return nil, fmt.Errorf("task stream closed")
			}
			if event.Task == nil || event.Task.ID != taskID {
				continue
			}
			if isFinalImageTaskStatus(event.Task.Status) {
				return event.Task, nil
			}
		}
	}
}

func (m *imageTaskManager) cancelTask(userID, id string) (*imageTaskView, error) {
	taskID := strings.TrimSpace(id)
	m.mu.Lock()
	task := m.tasks[taskID]
	if task == nil || task.UserID != userID {
		m.mu.Unlock()
		return nil, fmt.Errorf("task not found")
	}
	switch task.Status {
	case imageTaskStatusSucceeded, imageTaskStatusFailed, imageTaskStatusCancelled, imageTaskStatusExpired:
		view := m.buildTaskViewLocked(task)
		m.mu.Unlock()
		return view, nil
	case imageTaskStatusQueued:
		task.Status = imageTaskStatusCancelled
		task.FinishedAt = time.Now().UTC()
		for index := range task.Units {
			if task.Units[index].Status == imageTaskStatusQueued {
				task.Units[index].Status = imageTaskStatusCancelled
				task.Images[index].Status = "error"
				task.Images[index].Error = "任务已取消"
			}
		}
	default:
		now := time.Now().UTC()
		task.CancelRequested = true
		task.Status = imageTaskStatusCancelRequested
		for index := range task.Units {
			if task.Units[index].Status == imageTaskStatusQueued {
				task.Units[index].Status = imageTaskStatusCancelled
				task.Units[index].FinishedAt = now
				task.Images[index].Status = "error"
				task.Images[index].Error = "任务已取消"
				continue
			}
			if task.Units[index].Status == imageTaskStatusRunning && task.Units[index].Cancel != nil {
				task.Units[index].Cancel()
			}
		}
		if task.ActiveUnits == 0 {
			task.Status = imageTaskStatusCancelled
			task.FinishedAt = now
		}
	}
	cleanupAt := m.retentionDeadlineForTaskLocked(task)
	view := m.buildTaskViewLocked(task)
	snapshot := m.snapshotLocked(task.UserID)
	subscribers := m.subscriberChannelsLocked()
	m.mu.Unlock()

	m.broadcast(subscribers, imageTaskEvent{
		Type:     "task.upsert",
		Task:     view,
		Snapshot: snapshot,
	})
	if !cleanupAt.IsZero() {
		m.scheduleAfter(cleanupAt)
	}
	return view, nil
}

func (m *imageTaskManager) subscribe(userID string) (int, <-chan imageTaskEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextSubID++
	id := m.nextSubID
	ch := make(chan imageTaskEvent, 32)
	m.subscribers[id] = &imageTaskSubscriber{ch: ch, userID: userID}
	return id, ch
}

func (m *imageTaskManager) unsubscribe(id int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sub, ok := m.subscribers[id]
	if !ok {
		return
	}
	delete(m.subscribers, id)
	close(sub.ch)
}

func (m *imageTaskManager) triggerSchedule() {
	go m.schedule()
}

func (m *imageTaskManager) schedule() {
	m.scheduleMu.Lock()
	defer m.scheduleMu.Unlock()

	for {
		if !m.tryScheduleOne() {
			return
		}
	}
}

func (m *imageTaskManager) tryScheduleOne() bool {
	now := time.Now().UTC()
	m.mu.Lock()
	expiredViews := m.expireQueuedTasksLocked(now)
	if len(expiredViews) > 0 {
		subscribers := m.subscriberChannelsLocked()
		// Per-owner snapshot: each view's owner gets a snapshot scoped to its
		// own tasks, so counts never leak across tenants.
		snapshots := map[string]*imageTaskSnapshot{}
		for _, view := range expiredViews {
			owner := view.ownerUserID
			if _, ok := snapshots[owner]; !ok {
				snapshots[owner] = m.snapshotLocked(owner)
			}
		}
		m.mu.Unlock()
		for _, view := range expiredViews {
			m.broadcast(subscribers, imageTaskEvent{
				Type:     "task.upsert",
				Task:     view,
				Snapshot: snapshots[view.ownerUserID],
			})
		}
		return true
	}
	removedTasks := m.pruneRetainedTasksLocked(now)
	if len(removedTasks) > 0 {
		subscribers := m.subscriberChannelsLocked()
		snapshots := map[string]*imageTaskSnapshot{}
		for _, ref := range removedTasks {
			if _, ok := snapshots[ref.userID]; !ok {
				snapshots[ref.userID] = m.snapshotLocked(ref.userID)
			}
		}
		nextWakeAt := m.nextMaintenanceAtLocked(now)
		m.mu.Unlock()
		if !nextWakeAt.IsZero() {
			m.scheduleAfter(nextWakeAt)
		}
		for _, ref := range removedTasks {
			m.broadcast(subscribers, imageTaskEvent{
				Type:     "task.remove",
				TaskID:   ref.id,
				Snapshot: snapshots[ref.userID],
				userID:   ref.userID,
			})
		}
		return true
	}

	maxRunning := m.maxRunningLocked()
	if m.runningUnits >= maxRunning {
		updatedViews := make([]*imageTaskView, 0)
		for _, id := range m.order {
			task := m.tasks[id]
			if task == nil || task.Status != imageTaskStatusQueued {
				continue
			}
			previousReason := task.WaitingReason
			previousBlockers := append([]imageTaskBlocker(nil), task.Blockers...)
			_, retryAt := m.nextReadyQueuedUnitIndexLocked(task, now)
			if !retryAt.IsZero() {
				task.WaitingReason = imageTaskWaitingReasonRetryBackoff
				task.Blockers = []imageTaskBlocker{imageTaskRetryBackoffBlocker(now, retryAt)}
			} else {
				task.WaitingReason = imageTaskWaitingReasonGlobalConcurrency
				task.Blockers = []imageTaskBlocker{{Code: string(imageTaskWaitingReasonGlobalConcurrency), Detail: "等待全局并发槽位"}}
			}
			if previousReason != task.WaitingReason || !sameImageTaskBlockers(previousBlockers, task.Blockers) {
				updatedViews = append(updatedViews, m.buildTaskViewLocked(task))
			}
		}
		// Per-owner snapshots so tenant A never sees counts that include B's tasks.
		perOwner := map[string]*imageTaskSnapshot{}
		for _, view := range updatedViews {
			if _, ok := perOwner[view.ownerUserID]; !ok {
				perOwner[view.ownerUserID] = m.snapshotLocked(view.ownerUserID)
			}
		}
		subscribers := m.subscriberChannelsLocked()
		nextWakeAt := m.nextMaintenanceAtLocked(now)
		m.mu.Unlock()
		if !nextWakeAt.IsZero() {
			m.scheduleAfter(nextWakeAt)
		}
		for _, view := range updatedViews {
			m.broadcast(subscribers, imageTaskEvent{
				Type:     "task.upsert",
				Task:     view,
				Snapshot: perOwner[view.ownerUserID],
			})
		}
		return false
	}

	candidateIDs := make([]string, 0, len(m.order))
	for _, id := range m.order {
		task := m.tasks[id]
		if task == nil {
			continue
		}
		if task.Status != imageTaskStatusQueued && task.Status != imageTaskStatusRunning {
			continue
		}
		if task.CancelRequested {
			continue
		}
		if task.ActiveUnits >= maxImageTaskParallelUnitsPerTask {
			continue
		}
		unitIndex, _ := m.nextReadyQueuedUnitIndexLocked(task, now)
		if unitIndex < 0 {
			continue
		}
		candidateIDs = append(candidateIDs, id)
	}
	nextWakeAt := m.nextMaintenanceAtLocked(now)
	m.mu.Unlock()

	for _, id := range candidateIDs {
		task := m.copyTask(id)
		if task == nil {
			continue
		}
		lease, blocker, fatalErr := m.acquireLeaseForTask(task)
		if fatalErr != nil {
			m.failTask(id, fatalErr)
			return true
		}
		if lease == nil {
			m.updateTaskBlocker(id, blocker)
			continue
		}

		m.mu.Lock()
		current := m.tasks[id]
		if current == nil {
			m.mu.Unlock()
			if lease.release != nil {
				lease.release()
			}
			return false
		}
		unitIndex, retryAt := m.nextReadyQueuedUnitIndexLocked(current, time.Now().UTC())
		if unitIndex < 0 {
			m.mu.Unlock()
			if lease.release != nil {
				lease.release()
			}
			if !retryAt.IsZero() && (nextWakeAt.IsZero() || retryAt.Before(nextWakeAt)) {
				nextWakeAt = retryAt
			}
			continue
		}
		if m.runningUnits >= m.maxRunningLocked() || current.ActiveUnits >= maxImageTaskParallelUnitsPerTask {
			m.mu.Unlock()
			if lease.release != nil {
				lease.release()
			}
			return false
		}
		now := time.Now().UTC()
		// Carry the task's owner identity into the execution context so the
		// async path resolves the correct per-user CPA credential (§4.4). The
		// sync /v1 path gets userID from the session middleware; tasks run on a
		// background context, so we inject it here instead.
		runCtx, cancel := context.WithCancel(identity.WithUserID(context.Background(), current.UserID))
		if current.StartedAt.IsZero() {
			current.StartedAt = now
		}
		current.Status = imageTaskStatusRunning
		current.WaitingReason = imageTaskWaitingReasonNone
		current.Blockers = nil
		current.ActiveUnits++
		current.Units[unitIndex].Status = imageTaskStatusRunning
		current.Units[unitIndex].StartedAt = now
		current.Units[unitIndex].NextAttemptAt = time.Time{}
		current.Units[unitIndex].Cancel = cancel
		m.runningUnits++
		view := m.buildTaskViewLocked(current)
		snapshot := m.snapshotLocked(current.UserID)
		subscribers := m.subscriberChannelsLocked()
		m.mu.Unlock()

		m.broadcast(subscribers, imageTaskEvent{
			Type:     "task.upsert",
			Task:     view,
			Snapshot: snapshot,
		})

		go m.runUnit(id, unitIndex, lease, runCtx)
		return true
	}

	if !nextWakeAt.IsZero() {
		m.scheduleAfter(nextWakeAt)
	}
	return false
}

func (m *imageTaskManager) runUnit(taskID string, unitIndex int, lease *imageTaskLease, ctx context.Context) {
	images, err := m.server.executeImageTaskUnit(ctx, taskID, unitIndex, lease)
	if lease != nil && lease.release != nil {
		lease.release()
	}

	m.mu.Lock()
	task := m.tasks[taskID]
	if task == nil {
		m.mu.Unlock()
		m.triggerSchedule()
		return
	}
	now := time.Now().UTC()
	task.ActiveUnits--
	if task.ActiveUnits < 0 {
		task.ActiveUnits = 0
	}
	task.Units[unitIndex].Cancel = nil
	m.runningUnits--
	if m.runningUnits < 0 {
		m.runningUnits = 0
	}
	var deferredErr *imageTaskDeferredError
	if task.CancelRequested {
		task.Units[unitIndex].Status = imageTaskStatusCancelled
		task.Units[unitIndex].FinishedAt = now
		task.Units[unitIndex].Error = "任务已取消"
		task.Units[unitIndex].NextAttemptAt = time.Time{}
		task.Images[unitIndex].Status = "error"
		task.Images[unitIndex].Error = "任务已取消"
	} else if errors.As(err, &deferredErr) {
		task.Units[unitIndex].DeferredCount++
		if task.Units[unitIndex].DeferredCount > maxImageTaskDeferredAttempts {
			message := "临时失败重试次数过多，请稍后重试"
			if deferredErr != nil && strings.TrimSpace(deferredErr.Error()) != "" {
				message = fmt.Sprintf("%s：%s", message, strings.TrimSpace(deferredErr.Error()))
			}
			task.Units[unitIndex].FinishedAt = now
			task.Units[unitIndex].Status = imageTaskStatusFailed
			task.Units[unitIndex].Error = message
			task.Units[unitIndex].NextAttemptAt = time.Time{}
			task.Images[unitIndex].Status = "error"
			task.Images[unitIndex].Error = message
		} else {
			backoff := imageTaskRetryBackoffDuration(task.Units[unitIndex].DeferredCount)
			task.Units[unitIndex].Status = imageTaskStatusQueued
			task.Units[unitIndex].StartedAt = time.Time{}
			task.Units[unitIndex].FinishedAt = time.Time{}
			task.Units[unitIndex].Error = ""
			task.Units[unitIndex].NextAttemptAt = now.Add(backoff)
			task.Images[unitIndex].Status = "loading"
			task.Images[unitIndex].Error = ""
			blocker := imageTaskRetryBackoffBlocker(now, task.Units[unitIndex].NextAttemptAt)
			task.WaitingReason = imageTaskWaitingReason(blocker.Code)
			task.Blockers = []imageTaskBlocker{blocker}
		}
	} else if err != nil {
		task.Units[unitIndex].FinishedAt = now
		task.Units[unitIndex].Status = imageTaskStatusFailed
		task.Units[unitIndex].Error = err.Error()
		task.Images[unitIndex].Status = "error"
		task.Images[unitIndex].Error = err.Error()
	} else if len(images) > 0 {
		task.Units[unitIndex].FinishedAt = now
		task.Units[unitIndex].Status = imageTaskStatusSucceeded
		image := images[0]
		image.ID = task.Images[unitIndex].ID
		image.Status = "success"
		task.Images[unitIndex] = image
	}

	queuedUnits := 0
	runningUnits := 0
	failedUnits := 0
	cancelledUnits := 0
	for _, unit := range task.Units {
		switch unit.Status {
		case imageTaskStatusQueued:
			queuedUnits++
		case imageTaskStatusRunning:
			runningUnits++
		case imageTaskStatusFailed:
			failedUnits++
		case imageTaskStatusCancelled:
			cancelledUnits++
		}
	}

	switch {
	case task.CancelRequested && queuedUnits == 0 && runningUnits == 0:
		task.Status = imageTaskStatusCancelled
		task.FinishedAt = now
		task.WaitingReason = imageTaskWaitingReasonNone
		task.Blockers = nil
	case task.CancelRequested && runningUnits > 0:
		task.Status = imageTaskStatusCancelRequested
		task.FinishedAt = time.Time{}
		task.WaitingReason = imageTaskWaitingReasonNone
		task.Blockers = nil
	case runningUnits > 0:
		task.Status = imageTaskStatusRunning
		task.WaitingReason = imageTaskWaitingReasonNone
		task.Blockers = nil
	case queuedUnits > 0:
		task.Status = imageTaskStatusQueued
		task.FinishedAt = time.Time{}
	case failedUnits > 0:
		task.Status = imageTaskStatusFailed
		task.FinishedAt = now
		task.Error = firstNonEmpty(task.Images[unitIndex].Error, task.Error, "image task failed")
		task.WaitingReason = imageTaskWaitingReasonNone
		task.Blockers = nil
	case cancelledUnits == len(task.Units):
		task.Status = imageTaskStatusCancelled
		task.FinishedAt = now
		task.WaitingReason = imageTaskWaitingReasonNone
		task.Blockers = nil
	default:
		task.Status = imageTaskStatusSucceeded
		task.FinishedAt = now
		task.WaitingReason = imageTaskWaitingReasonNone
		task.Blockers = nil
	}

	view := m.buildTaskViewLocked(task)
	snapshot := m.snapshotLocked(task.UserID)
	subscribers := m.subscriberChannelsLocked()
	cleanupAt := m.retentionDeadlineForTaskLocked(task)
	m.mu.Unlock()

	m.broadcast(subscribers, imageTaskEvent{
		Type:     "task.upsert",
		Task:     view,
		Snapshot: snapshot,
	})
	if !cleanupAt.IsZero() {
		m.scheduleAfter(cleanupAt)
	}
	m.triggerSchedule()
}

func (m *imageTaskManager) failTask(taskID string, err error) {
	m.mu.Lock()
	task := m.tasks[taskID]
	if task == nil {
		m.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	task.Status = imageTaskStatusFailed
	task.Error = err.Error()
	task.FinishedAt = now
	for index := range task.Images {
		if task.Images[index].Status == "loading" {
			task.Images[index].Status = "error"
			task.Images[index].Error = err.Error()
		}
	}
	view := m.buildTaskViewLocked(task)
	snapshot := m.snapshotLocked(task.UserID)
	subscribers := m.subscriberChannelsLocked()
	cleanupAt := m.retentionDeadlineForTaskLocked(task)
	m.mu.Unlock()

	m.broadcast(subscribers, imageTaskEvent{
		Type:     "task.upsert",
		Task:     view,
		Snapshot: snapshot,
	})
	if !cleanupAt.IsZero() {
		m.scheduleAfter(cleanupAt)
	}
}

func (m *imageTaskManager) updateTaskBlocker(taskID string, blocker imageTaskBlocker) {
	m.mu.Lock()
	task := m.tasks[taskID]
	if task == nil || task.Status != imageTaskStatusQueued {
		m.mu.Unlock()
		return
	}
	task.WaitingReason = imageTaskWaitingReason(blocker.Code)
	task.Blockers = nil
	if blocker.Code != "" {
		task.Blockers = []imageTaskBlocker{blocker}
	}
	view := m.buildTaskViewLocked(task)
	snapshot := m.snapshotLocked(task.UserID)
	subscribers := m.subscriberChannelsLocked()
	m.mu.Unlock()

	m.broadcast(subscribers, imageTaskEvent{
		Type:     "task.upsert",
		Task:     view,
		Snapshot: snapshot,
	})
}

func (m *imageTaskManager) copyTask(taskID string) *imageTask {
	m.mu.Lock()
	defer m.mu.Unlock()
	task := m.tasks[taskID]
	if task == nil {
		return nil
	}
	copy := *task
	copy.Images = append([]imagehistory.Image(nil), task.Images...)
	copy.Units = append([]imageTaskUnit(nil), task.Units...)
	copy.SourceImages = append([]imageTaskSourceImage(nil), task.SourceImages...)
	copy.Blockers = append([]imageTaskBlocker(nil), task.Blockers...)
	return &copy
}

func (m *imageTaskManager) removeTaskIDFromOrderLocked(taskID string) {
	if len(m.order) == 0 {
		return
	}
	nextOrder := m.order[:0]
	for _, currentID := range m.order {
		if currentID == taskID {
			continue
		}
		nextOrder = append(nextOrder, currentID)
	}
	m.order = nextOrder
}

func (m *imageTaskManager) acquireLeaseForTask(task *imageTask) (*imageTaskLease, imageTaskBlocker, error) {
	// Multi-tenant CPA mode: execution resolves a per-user credential from the
	// task's userID (see executeImageTaskUnit), so there is no account-pool
	// lease to acquire. Return a marker lease; concurrency is bounded by the
	// task manager's runningUnits cap.
	_ = task
	return &imageTaskLease{}, imageTaskBlocker{}, nil
}

func (m *imageTaskManager) newTask(req createImageTaskRequest) (*imageTask, error) {
	id := firstNonEmpty(strings.TrimSpace(req.TaskID), strings.TrimSpace(req.TurnID))
	if id == "" {
		id = fmt.Sprintf("task-%d", time.Now().UnixNano())
	}
	prompt := strings.TrimSpace(req.Prompt)
	mode := strings.TrimSpace(req.Mode)
	if mode == "" {
		mode = "generate"
	}
	count := req.Count
	if count <= 0 {
		count = 1
	}
	if count > 8 {
		count = 8
	}
	sourceImages := make([]imageTaskSourceImage, 0, len(req.SourceImages))
	for index, source := range req.SourceImages {
		sourceImages = append(sourceImages, imageTaskSourceImage{
			ID:      firstNonEmpty(strings.TrimSpace(source.ID), fmt.Sprintf("%s-source-%d", id, index)),
			Role:    firstNonEmpty(strings.TrimSpace(source.Role), "image"),
			Name:    firstNonEmpty(strings.TrimSpace(source.Name), fmt.Sprintf("source-%d.png", index+1)),
			DataURL: strings.TrimSpace(source.DataURL),
			URL:     strings.TrimSpace(source.URL),
		})
	}

	var sourceReference *imageTaskSourceReference
	if req.SourceReference != nil {
		sourceReference = &imageTaskSourceReference{
			OriginalFileID:  strings.TrimSpace(req.SourceReference.OriginalFileID),
			OriginalGenID:   strings.TrimSpace(req.SourceReference.OriginalGenID),
			ConversationID:  strings.TrimSpace(req.SourceReference.ConversationID),
			ParentMessageID: strings.TrimSpace(req.SourceReference.ParentMessageID),
			SourceAccountID: strings.TrimSpace(req.SourceReference.SourceAccountID),
		}
	}

	requirement := imageTaskRequirement{}
	if sourceReference != nil && sourceReference.SourceAccountID != "" {
		requirement.SourceAccountID = sourceReference.SourceAccountID
	}

	createdAt := time.Now().UTC()
	images := make([]imagehistory.Image, 0, count)
	units := make([]imageTaskUnit, 0, count)
	for index := 0; index < count; index++ {
		images = append(images, imagehistory.Image{
			ID:     fmt.Sprintf("%s-%d", id, index),
			Status: "loading",
		})
		units = append(units, imageTaskUnit{
			Index:  index,
			Status: imageTaskStatusQueued,
		})
	}

	return &imageTask{
		ID:              id,
		UserID:          strings.TrimSpace(req.UserID),
		ConversationID:  strings.TrimSpace(req.ConversationID),
		TurnID:          strings.TrimSpace(req.TurnID),
		Source:          firstNonEmpty(strings.TrimSpace(req.Source), "workspace"),
		Mode:            mode,
		Prompt:          prompt,
		Model:           normalizeRequestedImageModel(req.Model, m.server.cfg.ChatGPT.Model),
		Count:           count,
		RetryImageIndex: req.RetryImageIndex,
		Size:            strings.TrimSpace(req.Size),
		Quality:         strings.TrimSpace(req.Quality),
		Background:      strings.TrimSpace(req.Background),
		ResponseFormat:  firstNonEmpty(strings.TrimSpace(req.ResponseFormat), "url"),
		SourceImages:    sourceImages,
		SourceReference: sourceReference,
		Requirement:     requirement,
		CreatedAt:       createdAt,
		Status:          imageTaskStatusQueued,
		Images:          images,
		Units:           units,
	}, nil
}

func (m *imageTaskManager) nextQueuedUnitIndexLocked(task *imageTask) int {
	for index := range task.Units {
		if task.Units[index].Status == imageTaskStatusQueued {
			return index
		}
	}
	return -1
}

func (m *imageTaskManager) nextReadyQueuedUnitIndexLocked(task *imageTask, now time.Time) (int, time.Time) {
	earliestRetryAt := time.Time{}
	for index := range task.Units {
		unit := task.Units[index]
		if unit.Status != imageTaskStatusQueued {
			continue
		}
		if !unit.NextAttemptAt.IsZero() && unit.NextAttemptAt.After(now) {
			if earliestRetryAt.IsZero() || unit.NextAttemptAt.Before(earliestRetryAt) {
				earliestRetryAt = unit.NextAttemptAt
			}
			continue
		}
		return index, time.Time{}
	}
	return -1, earliestRetryAt
}

func (m *imageTaskManager) queueTTL() time.Duration {
	if m == nil || m.server == nil || m.server.cfg == nil {
		return 10 * time.Minute
	}
	ttl := m.server.cfg.ImageTaskQueueTTL()
	if ttl <= 0 {
		return 10 * time.Minute
	}
	return ttl
}

func (m *imageTaskManager) initialQueueExpiryAt(task *imageTask) time.Time {
	if task == nil || !task.StartedAt.IsZero() || task.Status != imageTaskStatusQueued {
		return time.Time{}
	}
	return task.CreatedAt.Add(m.queueTTL())
}

func (m *imageTaskManager) retentionDeadlineForTaskLocked(task *imageTask) time.Time {
	if task == nil || !isFinalImageTaskStatus(task.Status) || task.FinishedAt.IsZero() {
		return time.Time{}
	}
	return task.FinishedAt.Add(imageTaskRetentionAfterFinish)
}

func (m *imageTaskManager) expireQueuedTasksLocked(now time.Time) []*imageTaskView {
	expired := make([]*imageTaskView, 0)
	for _, id := range m.order {
		task := m.tasks[id]
		if task == nil || task.Status != imageTaskStatusQueued || !task.StartedAt.IsZero() {
			continue
		}
		expiresAt := m.initialQueueExpiryAt(task)
		if expiresAt.IsZero() || expiresAt.After(now) {
			continue
		}
		task.Status = imageTaskStatusExpired
		task.Error = "图片任务排队超时，请稍后重试"
		task.FinishedAt = now
		task.WaitingReason = imageTaskWaitingReasonNone
		task.Blockers = nil
		for index := range task.Units {
			if task.Units[index].Status == imageTaskStatusQueued {
				task.Units[index].Status = imageTaskStatusCancelled
				task.Units[index].FinishedAt = now
				task.Units[index].Error = task.Error
				task.Images[index].Status = "error"
				task.Images[index].Error = task.Error
			}
		}
		expired = append(expired, m.buildTaskViewLocked(task))
	}
	return expired
}

func (m *imageTaskManager) nextWakeAtLocked(now time.Time) time.Time {
	nextWakeAt := time.Time{}
	for _, id := range m.order {
		task := m.tasks[id]
		if task == nil || task.Status != imageTaskStatusQueued {
			continue
		}
		if retryAt := m.taskNextRetryAtLocked(task, now); !retryAt.IsZero() {
			if nextWakeAt.IsZero() || retryAt.Before(nextWakeAt) {
				nextWakeAt = retryAt
			}
		}
		if expiresAt := m.initialQueueExpiryAt(task); !expiresAt.IsZero() && expiresAt.After(now) {
			if nextWakeAt.IsZero() || expiresAt.Before(nextWakeAt) {
				nextWakeAt = expiresAt
			}
		}
	}
	return nextWakeAt
}

func (m *imageTaskManager) nextCleanupAtLocked(now time.Time) time.Time {
	nextCleanupAt := time.Time{}
	for _, id := range m.order {
		task := m.tasks[id]
		if task == nil {
			continue
		}
		cleanupAt := m.retentionDeadlineForTaskLocked(task)
		if cleanupAt.IsZero() || !cleanupAt.After(now) {
			continue
		}
		if nextCleanupAt.IsZero() || cleanupAt.Before(nextCleanupAt) {
			nextCleanupAt = cleanupAt
		}
	}
	return nextCleanupAt
}

func (m *imageTaskManager) nextMaintenanceAtLocked(now time.Time) time.Time {
	nextWakeAt := m.nextWakeAtLocked(now)
	nextCleanupAt := m.nextCleanupAtLocked(now)
	switch {
	case nextWakeAt.IsZero():
		return nextCleanupAt
	case nextCleanupAt.IsZero():
		return nextWakeAt
	case nextCleanupAt.Before(nextWakeAt):
		return nextCleanupAt
	default:
		return nextWakeAt
	}
}

// removedTaskRef pairs a pruned task's ID with its owner so the removal can be
// broadcast only to that owner's subscribers.
type removedTaskRef struct {
	id     string
	userID string
}

func (m *imageTaskManager) pruneRetainedTasksLocked(now time.Time) []removedTaskRef {
	if len(m.order) == 0 {
		return nil
	}
	nextOrder := make([]string, 0, len(m.order))
	removed := make([]removedTaskRef, 0)
	for _, id := range m.order {
		task := m.tasks[id]
		if task == nil {
			continue
		}
		cleanupAt := m.retentionDeadlineForTaskLocked(task)
		if !cleanupAt.IsZero() && !cleanupAt.After(now) {
			removed = append(removed, removedTaskRef{id: id, userID: task.UserID})
			delete(m.tasks, id)
			continue
		}
		nextOrder = append(nextOrder, id)
	}
	m.order = nextOrder
	return removed
}

func (m *imageTaskManager) taskNextRetryAtLocked(task *imageTask, now time.Time) time.Time {
	_, retryAt := m.nextReadyQueuedUnitIndexLocked(task, now)
	return retryAt
}

func (m *imageTaskManager) maxRunningLocked() int {
	maxRunning, _, _ := m.server.cfg.ImageQueueConfig()
	if maxRunning <= 0 {
		maxRunning = 1
	}
	return maxRunning
}

func (m *imageTaskManager) buildTaskViewLocked(task *imageTask) *imageTaskView {
	queuePosition := 0
	if task.Status == imageTaskStatusQueued {
		position := 1
		for _, id := range m.order {
			if id == task.ID {
				queuePosition = position
				break
			}
			candidate := m.tasks[id]
			if candidate == nil {
				continue
			}
			for _, unit := range candidate.Units {
				if unit.Status == imageTaskStatusQueued {
					position++
				}
			}
		}
	}

	view := &imageTaskView{
		ownerUserID:     task.UserID,
		ID:              task.ID,
		ConversationID:  task.ConversationID,
		TurnID:          task.TurnID,
		Mode:            task.Mode,
		Status:          task.Status,
		CreatedAt:       task.CreatedAt.Format(time.RFC3339Nano),
		Count:           task.Count,
		RetryImageIndex: task.RetryImageIndex,
		QueuePosition:   queuePosition,
		WaitingReason:   task.WaitingReason,
		Blockers:        append([]imageTaskBlocker(nil), task.Blockers...),
		Images:          append([]imagehistory.Image(nil), task.Images...),
		Error:           task.Error,
		CancelRequested: task.CancelRequested,
	}
	if !task.StartedAt.IsZero() {
		view.StartedAt = task.StartedAt.Format(time.RFC3339Nano)
	}
	if !task.FinishedAt.IsZero() {
		view.FinishedAt = task.FinishedAt.Format(time.RFC3339Nano)
	}
	return view
}

func (m *imageTaskManager) snapshotLocked(userID string) *imageTaskSnapshot {
	queued := 0
	total := 0
	activeSources := imageTaskSourceSnapshot{}
	finalStatuses := imageTaskFinalStatusSnapshot{}
	for _, id := range m.order {
		task := m.tasks[id]
		if task == nil {
			continue
		}
		if task.UserID != userID {
			continue
		}
		total++
		queuedUnitsForTask := 0
		runningUnitsForTask := 0
		for _, unit := range task.Units {
			switch unit.Status {
			case imageTaskStatusQueued:
				queuedUnitsForTask++
			case imageTaskStatusRunning:
				runningUnitsForTask++
			}
		}
		queued += queuedUnitsForTask
		addImageTaskSourceCountN(
			&activeSources,
			task.Source,
			queuedUnitsForTask+runningUnitsForTask,
		)
		switch task.Status {
		case imageTaskStatusSucceeded:
			finalStatuses.Succeeded++
		case imageTaskStatusFailed:
			finalStatuses.Failed++
		case imageTaskStatusCancelled:
			finalStatuses.Cancelled++
		case imageTaskStatusExpired:
			finalStatuses.Expired++
		}
	}
	return &imageTaskSnapshot{
		Running:          m.runningUnits,
		MaxRunning:       m.maxRunningLocked(),
		Queued:           queued,
		Total:            total,
		ActiveSources:    activeSources,
		FinalStatuses:    finalStatuses,
		RetentionSeconds: int(imageTaskRetentionAfterFinish / time.Second),
	}
}

func (m *imageTaskManager) subscriberChannelsLocked() []*imageTaskSubscriber {
	subs := make([]*imageTaskSubscriber, 0, len(m.subscribers))
	for _, sub := range m.subscribers {
		subs = append(subs, sub)
	}
	return subs
}

// broadcast delivers an event only to subscribers that own it. event.userID is
// the task owner; a subscriber receives it only when its userID matches, so one
// tenant never sees another's task stream. Events with an empty owner (rare,
// e.g. legacy single-tenant) are delivered to all subscribers.
func (m *imageTaskManager) broadcast(subscribers []*imageTaskSubscriber, event imageTaskEvent) {
	// Derive the owning userID: explicit event.userID wins, otherwise fall back
	// to the task view's owner. This lets the many call sites that build an event
	// from a view stay unchanged — the owner is carried on the view.
	owner := event.userID
	if owner == "" && event.Task != nil {
		owner = event.Task.ownerUserID
	}
	for _, sub := range subscribers {
		if owner != "" && sub.userID != owner {
			continue
		}
		select {
		case sub.ch <- event:
		default:
		}
	}
}

func (m *imageTaskManager) scheduleAfter(when time.Time) {
	if when.IsZero() {
		return
	}
	delay := time.Until(when)
	if delay < 0 {
		delay = 0
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.scheduleAt.IsZero() && !when.Before(m.scheduleAt) {
		return
	}
	if m.scheduleTimer != nil {
		m.scheduleTimer.Stop()
	}
	m.scheduleAt = when
	m.scheduleTimer = time.AfterFunc(delay, func() {
		m.mu.Lock()
		m.scheduleTimer = nil
		m.scheduleAt = time.Time{}
		m.mu.Unlock()
		m.triggerSchedule()
	})
}

func imageTaskRetryBackoffDuration(attempt int) time.Duration {
	if attempt <= 0 {
		return imageTaskRetryBackoffBase
	}
	backoff := imageTaskRetryBackoffBase
	for step := 1; step < attempt; step++ {
		backoff *= 2
		if backoff >= imageTaskRetryBackoffMax {
			return imageTaskRetryBackoffMax
		}
	}
	if backoff > imageTaskRetryBackoffMax {
		return imageTaskRetryBackoffMax
	}
	return backoff
}

func imageTaskRetryBackoffBlocker(now, nextAttemptAt time.Time) imageTaskBlocker {
	if nextAttemptAt.IsZero() {
		return imageTaskBlocker{
			Code:   string(imageTaskWaitingReasonRetryBackoff),
			Detail: "临时失败，稍后自动重试",
		}
	}
	waitFor := time.Until(nextAttemptAt)
	if !now.IsZero() {
		waitFor = nextAttemptAt.Sub(now)
	}
	if waitFor < time.Second {
		waitFor = time.Second
	}
	return imageTaskBlocker{
		Code:   string(imageTaskWaitingReasonRetryBackoff),
		Detail: fmt.Sprintf("临时失败，约 %s 后自动重试", formatRetryBackoff(waitFor)),
	}
}

func formatRetryBackoff(delay time.Duration) string {
	if delay < time.Second {
		return "1 秒"
	}
	seconds := int(delay.Round(time.Second) / time.Second)
	if seconds < 60 {
		return fmt.Sprintf("%d 秒", seconds)
	}
	minutes := seconds / 60
	remainSeconds := seconds % 60
	if remainSeconds == 0 {
		return fmt.Sprintf("%d 分钟", minutes)
	}
	return fmt.Sprintf("%d 分 %d 秒", minutes, remainSeconds)
}

func addImageTaskSourceCount(target *imageTaskSourceSnapshot, source string) {
	addImageTaskSourceCountN(target, source, 1)
}

func addImageTaskSourceCountN(target *imageTaskSourceSnapshot, source string, count int) {
	if target == nil {
		return
	}
	if count <= 0 {
		return
	}
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "compat":
		target.Compat += count
	default:
		target.Workspace += count
	}
}

func sameImageTaskBlockers(left, right []imageTaskBlocker) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func requiresPaidGenerateTask(size string) bool {
	normalized := normalizeGenerateImageSize(size)
	return strings.EqualFold(normalized, "2048x2048") ||
		strings.EqualFold(normalized, "2880x2880") ||
		strings.EqualFold(normalized, "2048x1536") ||
		strings.EqualFold(normalized, "3264x2448") ||
		strings.EqualFold(normalized, "2160x1440") ||
		strings.EqualFold(normalized, "3456x2304") ||
		strings.EqualFold(normalized, "2560x1440") ||
		strings.EqualFold(normalized, "3840x2160") ||
		strings.EqualFold(normalized, "3360x1440") ||
		strings.EqualFold(normalized, "3808x1632") ||
		strings.EqualFold(normalized, "1440x2560") ||
		strings.EqualFold(normalized, "2160x3840")
}

func writeSSEEvent(w http.ResponseWriter, event imageTaskEvent) error {
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
		return err
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func isFinalImageTaskStatus(status imageTaskStatus) bool {
	switch status {
	case imageTaskStatusSucceeded, imageTaskStatusFailed, imageTaskStatusCancelled, imageTaskStatusExpired:
		return true
	default:
		return false
	}
}
