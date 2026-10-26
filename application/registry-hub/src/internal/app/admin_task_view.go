package app

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

var errAdminTaskOwnerSession = errors.New("owner session does not belong to task")

const adminProtectionReadAttempts = 3

type adminProtectionSessionNotFound struct {
	sessionID string
}

func (e *adminProtectionSessionNotFound) Error() string {
	return "protection session disappeared while reading task state"
}

func (e *adminProtectionSessionNotFound) Unwrap() error {
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
	originalTask := task
	for attempt := 0; attempt < adminProtectionReadAttempts; attempt++ {
		view, err := s.taskAdminView(ctx, task)
		if err != nil {
			return adminTaskView{}, err
		}
		task = view.Task
		view.Graph, view.GraphNextToken = pageAdminMap(task.Graph, options.graphToken, options.limit)
		view.Checkpoints, view.CheckpointsNextToken = pageAdminMap(task.Checkpoints, options.checkpointsToken, options.limit)
		nodes, next, err := s.records.ListGraphNodes(ctx, task.ID, options.graphNodesToken, options.limit)
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
				options.ownerSessionID = ""
				options.ownerToken = ""
				task, err = s.tasks.Get(ctx, task.ID)
				if err != nil {
					return adminTaskView{}, err
				}
				continue
			}
			return adminTaskView{}, errAdminTaskOwnerSession
		}
		view.Owners, view.OwnersNextToken, err = s.gate.ListProtectionSessionOwners(ctx, ownerSessionID, options.ownerToken, options.ownerLimit)
		if errors.Is(err, storage.ErrNotFound) {
			task, err = s.tasks.Get(ctx, task.ID)
			if err != nil {
				return adminTaskView{}, err
			}
			if !adminTaskReferencesSession(task, ownerSessionID) {
				if options.ownerSessionID == ownerSessionID {
					options.ownerSessionID = ""
					options.ownerToken = ""
				}
				continue
			}
			if err := waitAdminProtectionRetry(ctx, attempt); err != nil {
				return adminTaskView{}, err
			}
			continue
		}
		if err != nil {
			return adminTaskView{}, err
		}
		view.OwnerSessionID = ownerSessionID
		return view, nil
	}
	return adminTaskView{}, &adminProtectionSessionNotFound{sessionID: options.ownerSessionID}
}

func (s *Service) taskAdminView(ctx context.Context, task tasks.Task) (adminTaskView, error) {
	for attempt := 0; attempt < adminProtectionReadAttempts; attempt++ {
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
		for _, id := range ids {
			status, err := s.gate.ProtectionSessionStatus(ctx, id)
			if errors.Is(err, storage.ErrNotFound) {
				missingSession = id
				break
			}
			if err != nil {
				return adminTaskView{}, err
			}
			view.ProtectionSessions = append(view.ProtectionSessions, status)
		}
		if missingSession == "" {
			sort.Slice(view.ProtectionSessions, func(i, j int) bool {
				return view.ProtectionSessions[i].ID < view.ProtectionSessions[j].ID
			})
			return view, nil
		}
		latest, err := s.tasks.Get(ctx, task.ID)
		if err != nil {
			return adminTaskView{}, err
		}
		task = latest
		if adminTaskReferencesSession(latest, missingSession) {
			if err := waitAdminProtectionRetry(ctx, attempt); err != nil {
				return adminTaskView{}, err
			}
		}
	}
	return adminTaskView{}, &adminProtectionSessionNotFound{}
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

func waitAdminProtectionRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(attempt+1) * 10 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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
