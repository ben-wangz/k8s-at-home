package app

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"

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
	view, err := s.taskAdminView(ctx, task)
	if err != nil {
		return adminTaskView{}, err
	}
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
	if ownerSessionID != "" {
		found := false
		for _, session := range view.ProtectionSessions {
			if session.ID == ownerSessionID {
				found = true
				break
			}
		}
		if !found {
			return adminTaskView{}, errAdminTaskOwnerSession
		}
		view.Owners, view.OwnersNextToken, err = s.gate.ListProtectionSessionOwners(ctx, ownerSessionID, options.ownerToken, options.ownerLimit)
		if err != nil {
			return adminTaskView{}, err
		}
		view.OwnerSessionID = ownerSessionID
	}
	return view, nil
}

func (s *Service) taskAdminView(ctx context.Context, task tasks.Task) (adminTaskView, error) {
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
	for _, id := range ids {
		status, err := s.gate.ProtectionSessionStatus(ctx, id)
		if err != nil {
			return adminTaskView{}, err
		}
		view.ProtectionSessions = append(view.ProtectionSessions, status)
	}
	sort.Slice(view.ProtectionSessions, func(i, j int) bool {
		return view.ProtectionSessions[i].ID < view.ProtectionSessions[j].ID
	})
	return view, nil
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
