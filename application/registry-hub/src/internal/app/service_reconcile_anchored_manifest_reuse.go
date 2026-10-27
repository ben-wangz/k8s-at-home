package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

type anchoredManifestReuseCursor struct {
	TaskToken   string
	TaskIndex   int
	GraphTaskID string
	GraphToken  string
}

func (s *Service) reconcileAnchoredManifestReusePage(ctx context.Context, cursor anchoredManifestReuseCursor) (anchoredManifestReuseCursor, bool, error) {
	items, next, err := s.tasks.List(ctx, "", cursor.TaskToken, 100)
	if err != nil {
		return cursor, false, err
	}
	for index := cursor.TaskIndex; index < len(items); index++ {
		task := items[index]
		if task.Kind != tasks.RootKind || task.Status != tasks.WaitingDeps || task.WaitReason != "dependencies_unavailable" || task.CancelRequested {
			continue
		}
		graphToken := ""
		if cursor.GraphTaskID == task.ID && cursor.TaskIndex == index {
			graphToken = cursor.GraphToken
		}
		nodes, graphNext, err := s.records.ListGraphNodes(ctx, task.ID, graphToken, 500)
		if err != nil {
			return cursor, false, err
		}
		for _, node := range nodes {
			if node.Role != tasks.GraphManifest {
				continue
			}
			eligible, err := s.graphNodeNeedsAnchoredReuseReconciliation(ctx, task, node)
			if err != nil {
				return cursor, false, err
			}
			if !eligible {
				continue
			}
			if _, err := s.tasks.Wake(ctx, task.ID); err != nil {
				if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrNotFound) || errors.Is(err, tasks.ErrCancelled) {
					break
				}
				latest, readErr := s.tasks.Get(ctx, task.ID)
				if errors.Is(readErr, storage.ErrNotFound) || readErr == nil && (latest.CancelRequested || latest.Status != tasks.WaitingDeps || latest.WaitReason != "dependencies_unavailable") {
					break
				}
				return cursor, false, err
			}
			break
		}
		if graphNext != "" {
			return anchoredManifestReuseCursor{TaskToken: cursor.TaskToken, TaskIndex: index, GraphTaskID: task.ID, GraphToken: graphNext}, false, nil
		}
		cursor.GraphTaskID = ""
		cursor.GraphToken = ""
	}
	if next == "" {
		return anchoredManifestReuseCursor{}, true, nil
	}
	return anchoredManifestReuseCursor{TaskToken: next}, false, nil
}

func (s *Service) graphNodeNeedsAnchoredReuseReconciliation(ctx context.Context, task tasks.Task, node tasks.GraphNode) (bool, error) {
	anchored, err := s.graphManifestIsCurrent(ctx, task, node)
	if err != nil || anchored {
		return false, err
	}
	return s.anchoredReuseManifestClosurePresent(ctx, task, node, make(map[string]bool), 0)
}

func (s *Service) graphManifestIsCurrent(ctx context.Context, task tasks.Task, node tasks.GraphNode) (bool, error) {
	if node.LocalState != "anchored" || node.ContentGeneration == nil || node.GCEpoch != s.gate.State().Epoch {
		return false, nil
	}
	generation, err := s.generation(ctx, task.Authority, task.Repository, node.Digest)
	if err != nil {
		return false, err
	}
	if *node.ContentGeneration != generation.Generation {
		return false, nil
	}
	if _, err := s.records.FindLocalObservation(ctx, task.Authority, task.Repository, tasks.GraphManifest, node.Digest, node.ActualSize, generation.Generation, node.GCEpoch); errors.Is(err, storage.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) anchoredReuseManifestClosurePresent(ctx context.Context, task tasks.Task, node tasks.GraphNode, visited map[string]bool, depth int) (bool, error) {
	if depth > s.cfg.Limits.ClosureDepth || visited[node.Digest] {
		return false, nil
	}
	visited[node.Digest] = true
	defer delete(visited, node.Digest)
	scopeID, err := protectionScopeID(task.Authority, task.Repository, node.Digest, task.Generation)
	if err != nil {
		return false, err
	}
	sessionID := stagedSessionID(scopeID)
	session, err := s.gate.readProtectionSession(ctx, sessionID)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	terminalAnchored := session.State == "anchored" || session.State == "released" && session.TerminalReason == "anchored"
	if session.ID != sessionID || session.ScopeID != scopeID || !terminalAnchored || session.PendingOperations != 0 || session.UncertainRemoteResult || session.RemoteWrite != nil {
		return false, nil
	}
	state := s.gate.State()
	if state.State != "open" || state.Exclusive || state.RecoveryBarrier {
		return false, nil
	}
	epoch := state.Epoch
	processor := &solidifier{service: s, cfg: s.cfg, registry: s.registry, gate: s.gate}
	manifest := contentRef{digest: node.Digest, mediaType: node.MediaType, size: node.DeclaredSize}
	if _, _, err := processor.readDistributionManifest(ctx, &task, manifest); errors.Is(err, storage.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	token := ""
	for {
		edges, next, err := s.records.ListDependencyEdges(ctx, task.ID, token, 500)
		if err != nil {
			return false, err
		}
		for _, edge := range edges {
			if edge.ParentRole != tasks.GraphManifest || edge.ParentDigest != node.Digest {
				continue
			}
			switch edge.ChildRole {
			case tasks.GraphBlob:
				blob := contentRef{digest: edge.ChildDigest, mediaType: edge.Descriptor.MediaType, size: edge.Descriptor.Size}
				present, err := processor.headDistributionBlob(ctx, &task, blob)
				if err != nil {
					return false, err
				}
				if !present {
					return false, nil
				}
			case tasks.GraphManifest:
				child, err := s.records.GetGraphNode(ctx, task.ID, tasks.GraphManifest, edge.ChildDigest)
				if errors.Is(err, storage.ErrNotFound) {
					return false, nil
				}
				if err != nil {
					return false, err
				}
				childAnchored, err := s.graphManifestIsCurrent(ctx, task, child)
				if err != nil {
					return false, err
				}
				if childAnchored {
					continue
				}
				present, err := s.anchoredReuseManifestClosurePresent(ctx, task, child, visited, depth+1)
				if err != nil || !present {
					return false, err
				}
			}
		}
		if next == "" {
			break
		}
		token = next
	}
	return processor.observationEpochOpen(epoch), nil
}
