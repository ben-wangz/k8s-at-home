package app

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

var errAdminTaskOwnerSession = errors.New("owner session does not belong to task")

type adminTaskDetailOptions struct {
	limit            int
	graphToken       string
	graphNodesToken  string
	checkpointsToken string
	ownerSessionID   string
	ownerToken       string
	ownerLimit       int
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
