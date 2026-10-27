package app

import (
	"context"
	"errors"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

var errAdminTaskOwnerSession = errors.New("owner session does not belong to task")

const (
	adminProtectionReadTimeout      = 5 * time.Second
	adminProtectionReadMaxRetries   = 8
	adminProtectionReadInitialDelay = 25 * time.Millisecond
	adminProtectionReadMaximumDelay = 400 * time.Millisecond
)

type adminProtectionReadFailure struct {
	sessionID  string
	phase      string
	retryCount int
	reason     string
	cause      error
}

func (e *adminProtectionReadFailure) Error() string {
	if e.cause != nil {
		return e.cause.Error()
	}
	return "protection session disappeared while reading task state"
}

func (e *adminProtectionReadFailure) Unwrap() error {
	if e.cause != nil {
		return e.cause
	}
	return storage.ErrNotFound
}

type adminTaskDetailOptions struct {
	limit            int
	graphToken       string
	graphNodesToken  string
	checkpointsToken string
	ownerSessionID   string
	ownerToken       string
	ownerLimit       int
}

type adminProtectionReadRetry struct {
	retries int
}

func parseAdminTaskDetailOptions(query url.Values) (adminTaskDetailOptions, error) {
	if !onlyQueryKeys(query, "graphLimit", "graphToken", "graphNodesToken", "checkpointsToken", "ownerSessionID", "ownerToken", "ownerLimit") {
		return adminTaskDetailOptions{}, errors.New("task detail query is invalid")
	}
	options := adminTaskDetailOptions{
		limit: 100, graphToken: query.Get("graphToken"), graphNodesToken: query.Get("graphNodesToken"),
		checkpointsToken: query.Get("checkpointsToken"), ownerSessionID: query.Get("ownerSessionID"),
		ownerToken: query.Get("ownerToken"), ownerLimit: 100,
	}
	if raw := query.Get("graphLimit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			return adminTaskDetailOptions{}, errors.New("task graph page size is invalid")
		}
		options.limit = limit
	}
	if raw := query.Get("ownerLimit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			return adminTaskDetailOptions{}, errors.New("task owner page size is invalid")
		}
		options.ownerLimit = limit
	}
	if !validAdminPageToken(options.graphToken, 512) || !validAdminPageToken(options.graphNodesToken, 4096) || !validAdminPageToken(options.checkpointsToken, 512) || !validAdminPageToken(options.ownerSessionID, 32) || options.ownerSessionID != "" && !validGateID(options.ownerSessionID) || !validAdminPageToken(options.ownerToken, 4096) {
		return adminTaskDetailOptions{}, errors.New("task detail page token is invalid")
	}
	return options, nil
}

func validAdminPageToken(token string, max int) bool {
	return len(token) <= max && !strings.ContainsAny(token, "\r\n\x00")
}

func (s *Service) taskDetailView(ctx context.Context, task tasks.Task, options adminTaskDetailOptions) (adminTaskView, error) {
	readCtx, cancel := context.WithTimeout(ctx, adminProtectionReadTimeout)
	defer cancel()
	retry := &adminProtectionReadRetry{}
	originalTask := task
	for {
		view, err := s.taskAdminViewWithRetry(readCtx, task, retry)
		if err != nil {
			return adminTaskView{}, err
		}
		task = view.Task
		view.Graph, view.GraphNextToken = pageAdminMap(task.Graph, options.graphToken, options.limit)
		view.Checkpoints, view.CheckpointsNextToken = pageAdminMap(task.Checkpoints, options.checkpointsToken, options.limit)
		nodes, next, err := s.records.ListGraphNodes(readCtx, task.ID, options.graphNodesToken, options.limit)
		if err != nil {
			return adminTaskView{}, err
		}
		view.GraphNodes = nodes
		view.GraphNodesNextToken = next
		ownerSessionID := options.ownerSessionID
		if ownerSessionID == "" && len(view.ProtectionSessions) > 0 {
			ownerSessionID = view.ProtectionSessions[0].ID
		}
		if ownerSessionID == "" {
			return view, nil
		}
		if !adminViewHasProtectionSession(view, ownerSessionID) {
			if options.ownerSessionID == ownerSessionID && adminTaskReferencesSession(originalTask, ownerSessionID) {
				return view, nil
			}
			return adminTaskView{}, errAdminTaskOwnerSession
		}
		view.Owners, view.OwnersNextToken, err = s.gate.ListProtectionSessionOwners(readCtx, ownerSessionID, options.ownerToken, options.ownerLimit)
		if errors.Is(err, storage.ErrNotFound) {
			task, err = s.tasks.Get(readCtx, task.ID)
			if err != nil {
				return adminTaskView{}, adminProtectionReadError(ownerSessionID, "owner_task_refresh", retry.retries, err)
			}
			if !adminTaskReferencesSession(task, ownerSessionID) {
				continue
			}
			status, statusErr := s.gate.ProtectionSessionStatus(readCtx, ownerSessionID)
			if statusErr == nil && status.State == "released" {
				continue
			}
			if statusErr != nil && !errors.Is(statusErr, storage.ErrNotFound) {
				return adminTaskView{}, adminProtectionReadError(ownerSessionID, "owner_session_status", retry.retries, statusErr)
			}
			if err := retry.wait(readCtx, ownerSessionID, "owner_list"); err != nil {
				return adminTaskView{}, err
			}
			continue
		}
		if err != nil {
			return adminTaskView{}, adminProtectionReadError(ownerSessionID, "owner_list", retry.retries, err)
		}
		view.OwnerSessionID = ownerSessionID
		return view, nil
	}
}

func (s *Service) taskAdminView(ctx context.Context, task tasks.Task) (adminTaskView, error) {
	readCtx, cancel := context.WithTimeout(ctx, adminProtectionReadTimeout)
	defer cancel()
	return s.taskAdminViewWithRetry(readCtx, task, &adminProtectionReadRetry{})
}

func (s *Service) taskAdminViewWithRetry(ctx context.Context, task tasks.Task, retry *adminProtectionReadRetry) (adminTaskView, error) {
	for {
		view := taskAdminView(task)
		if s.gate == nil {
			if len(task.SessionIDs) > 0 || task.ProtectionSessionID != "" {
				return adminTaskView{}, errors.New("task protection state is unavailable")
			}
			return view, nil
		}
		ids := append([]string(nil), task.SessionIDs...)
		if task.ProtectionSessionID != "" {
			ids = append(ids, task.ProtectionSessionID)
		}
		ids = uniqueSessionIDs(ids)
		var missingSession string
		releasedSessions := make(map[string]struct{})
		for _, id := range ids {
			status, err := s.gate.ProtectionSessionStatus(ctx, id)
			if errors.Is(err, storage.ErrNotFound) {
				missingSession = id
				break
			}
			if err != nil {
				return adminTaskView{}, adminProtectionReadError(id, "session_status", retry.retries, err)
			}
			if status.State == "released" {
				releasedSessions[id] = struct{}{}
				continue
			}
			view.ProtectionSessions = append(view.ProtectionSessions, status)
		}
		if missingSession == "" {
			normalizedTask, err := adminTaskWithoutReleasedProtection(task, releasedSessions, view.ProtectionSessions)
			if err != nil {
				return adminTaskView{}, err
			}
			view.Task = normalizedTask
			view.GCProtectionActive = normalizedTask.GCProtected || len(normalizedTask.SessionIDs) > 0 || normalizedTask.ProtectionSessionID != "" || normalizedTask.RecoveryReason != ""
			sort.Slice(view.ProtectionSessions, func(i, j int) bool {
				return view.ProtectionSessions[i].ID < view.ProtectionSessions[j].ID
			})
			return view, nil
		}
		latest, err := s.tasks.Get(ctx, task.ID)
		if err != nil {
			return adminTaskView{}, adminProtectionReadError(missingSession, "task_refresh", retry.retries, err)
		}
		task = latest
		if adminTaskReferencesSession(latest, missingSession) {
			if err := retry.wait(ctx, missingSession, "session_status"); err != nil {
				return adminTaskView{}, err
			}
		}
	}
}

func adminTaskWithoutReleasedProtection(task tasks.Task, released map[string]struct{}, activeSessions []protectionSessionStatus) (tasks.Task, error) {
	if len(released) == 0 {
		return task, nil
	}
	if len(task.SessionIDs) > 0 {
		sessionIDs := make([]string, 0, len(task.SessionIDs))
		for _, id := range task.SessionIDs {
			if _, ok := released[id]; !ok {
				sessionIDs = append(sessionIDs, id)
			}
		}
		task.SessionIDs = sessionIDs
		if len(task.SessionIDs) == 0 {
			task.SessionIDs = nil
		}
	}
	if _, ok := released[task.ProtectionSessionID]; ok {
		task.ProtectionSessionID = ""
	}
	if len(task.SessionIDs) == 0 && task.ProtectionSessionID == "" {
		if task.RecoveryReason == "" {
			task.GCProtected = false
			task.ReservedBytes = 0
		} else {
			task.GCProtected = true
		}
		return task, nil
	}
	var reserved int64
	for _, status := range activeSessions {
		session := protectionSessionRecord{
			ID: status.ID, ScopeID: status.ScopeID, ReservationClass: status.ReservationClass, ReservedBytes: status.ReservedBytes,
		}
		reservedBytes, err := taskProtectionReservationBytes(&task, status.ID, session)
		if err != nil {
			return tasks.Task{}, err
		}
		if reservedBytes > math.MaxInt64-reserved {
			return tasks.Task{}, errors.New("task protection reservation overflows")
		}
		reserved += reservedBytes
	}
	task.ReservedBytes = reserved
	task.GCProtected = true
	return task, nil
}

func adminTaskReferencesSession(task tasks.Task, sessionID string) bool {
	if task.ProtectionSessionID == sessionID {
		return true
	}
	for _, id := range task.SessionIDs {
		if id == sessionID {
			return true
		}
	}
	return false
}

func adminViewHasProtectionSession(view adminTaskView, sessionID string) bool {
	for _, session := range view.ProtectionSessions {
		if session.ID == sessionID {
			return true
		}
	}
	return false
}

func (r *adminProtectionReadRetry) wait(ctx context.Context, sessionID, phase string) error {
	if r.retries >= adminProtectionReadMaxRetries {
		return &adminProtectionReadFailure{
			sessionID: sessionID, phase: phase, retryCount: r.retries,
			reason: "retry_limit", cause: storage.ErrNotFound,
		}
	}
	delay := adminProtectionReadInitialDelay << r.retries
	if delay > adminProtectionReadMaximumDelay {
		delay = adminProtectionReadMaximumDelay
	}
	r.retries++
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return adminProtectionReadError(sessionID, phase, r.retries, ctx.Err())
	case <-timer.C:
		return nil
	}
}

func adminProtectionReadError(sessionID, phase string, retries int, cause error) error {
	reason := "storage_error"
	if errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, context.Canceled) {
		reason = "context_done"
	}
	return &adminProtectionReadFailure{
		sessionID: sessionID, phase: phase, retryCount: retries, reason: reason, cause: cause,
	}
}

func (s *Service) logAdminTaskViewFailure(taskID string, err error) {
	if s.logger == nil {
		return
	}
	var readFailure *adminProtectionReadFailure
	if errors.As(err, &readFailure) {
		s.logger.Warn("admin task protection view failed",
			"task_id", taskID, "session_id", readFailure.sessionID,
			"phase", readFailure.phase, "retry_count", readFailure.retryCount,
			"reason", readFailure.reason, "error", readFailure.cause)
		return
	}
	s.logger.Warn("admin task protection view failed", "task_id", taskID, "phase", "protection_state_read", "error", err)
}

func pageAdminMap[T any](source map[string]T, after string, limit int) (map[string]T, string) {
	keys := make([]string, 0, len(source))
	for key := range source {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	page := make(map[string]T, min(len(keys), limit))
	last := ""
	var next string
	for _, key := range keys {
		if key <= after {
			continue
		}
		if len(page) == limit {
			next = last
			break
		}
		page[key] = source[key]
		last = key
	}
	return page, next
}
