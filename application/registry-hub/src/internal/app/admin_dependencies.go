package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"strings"
	"time"
)

type dependencyView struct {
	ParentDigest         string            `json:"parentDigest,omitempty"`
	ParentDigests        []string          `json:"parentDigests,omitempty"`
	ParentEdgesScope     string            `json:"parentEdgesScope"`
	ParentEdgesTruncated bool              `json:"parentEdgesTruncated"`
	Digest               string            `json:"digest"`
	Role                 string            `json:"role"`
	DeclaredBytes        int64             `json:"declaredBytes"`
	Platform             json.RawMessage   `json:"platform,omitempty"`
	CacheState           string            `json:"cacheState"`
	RegistryState        string            `json:"registryState"`
	IntentID             string            `json:"intentID,omitempty"`
	TaskID               string            `json:"taskID,omitempty"`
	Reason               string            `json:"reason"`
	NextAction           string            `json:"nextAction"`
	CheckedAt            time.Time         `json:"checkedAt"`
	EvidenceSource       string            `json:"evidenceSource"`
	ReadErrors           map[string]string `json:"readErrors,omitempty"`
	IntentURL            string            `json:"intentURL,omitempty"`
	ActivationWorkerURL  string            `json:"activationWorkerURL,omitempty"`
	NotificationWork     *responsibleWork  `json:"notificationWork,omitempty"`
	NotificationCoverage string            `json:"notificationCoverage,omitempty"`
	NotificationsURL     string            `json:"notificationsURL,omitempty"`
	NextAttemptAt        time.Time         `json:"nextAttemptAt"`
	LastProgressAt       time.Time         `json:"lastProgressAt"`
	CacheMediaType       string            `json:"cacheMediaType,omitempty"`
	CacheVersion         string            `json:"cacheVersion,omitempty"`
	IntentCacheVersion   string            `json:"intentCacheVersion,omitempty"`
	CacheReason          string            `json:"cacheReason,omitempty"`
}

type dependencyPage struct {
	diagnosticPage[dependencyView]
	BlockingInPage          int    `json:"blockingInPage"`
	KnownMissingBytesInPage int64  `json:"knownMissingBytesInPage"`
	TotalScope              string `json:"totalScope"`
}

func (s *Service) serveTaskDependencies(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/api/v1/tasks/"), "/dependencies")
	if !validGateID(id) {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	_, cursor, limit, err := s.diagnosticQuery(request)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	task, err := s.tasks.Get(ctx, id)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	edges, next, err := s.records.ListDependencyEdges(ctx, id, cursor.Storage, limit)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	page := dependencyPage{diagnosticPage: diagnosticPage[dependencyView]{Items: []dependencyView{}, ObservedAt: time.Now().UTC(), Consistency: "live", Scanned: len(edges)}, TotalScope: "page; continue until nextToken is absent"}
	missing := map[string]bool{}
	for _, edge := range edges {
		view := s.taskDependency(ctx, task, edge)
		page.Items = append(page.Items, view)
		if view.RegistryState != "verified" {
			page.BlockingInPage++
			if view.CacheState == "missing" && edge.ChildRole == tasks.GraphBlob && view.DeclaredBytes > 0 && !missing[edge.ChildDigest] {
				page.KnownMissingBytesInPage += view.DeclaredBytes
				missing[edge.ChildDigest] = true
			}
		}
	}
	if next != "" {
		cursor.Storage = next
		page.NextToken = s.diagnosticToken(cursor)
	}
	writeJSON(writer, http.StatusOK, page)
}

func (s *Service) taskDependency(ctx context.Context, task tasks.Task, edge tasks.DependencyEdge) dependencyView {
	view := dependencyView{ParentDigest: edge.ParentDigest, Digest: edge.ChildDigest, Role: string(edge.ChildRole), DeclaredBytes: edge.Descriptor.Size, Platform: edge.Descriptor.Platform, CacheState: "unknown", RegistryState: "unknown", Reason: "evidence_unknown", NextAction: "await_verification", CheckedAt: time.Now().UTC(), EvidenceSource: "persisted_graph_and_cache_metadata; no registry probe", ReadErrors: map[string]string{}}
	view.ParentDigests, view.ParentEdgesScope = []string{edge.ParentDigest}, "persisted_task_graph"
	inspection, intent, intentErr, err := s.inspectDependencyCache(ctx, task, edge)
	view.CacheMediaType, view.CacheVersion, view.CacheReason = inspection.MediaType, inspection.DataVersion, inspection.Reason
	if intentErr != nil && !errors.Is(intentErr, storage.ErrNotFound) {
		view.ReadErrors["intent"] = diagnosticReason(intentErr)
	}
	if err != nil {
		view.ReadErrors["cache"] = diagnosticReason(err)
	} else if inspection.Present && inspection.Size != edge.Descriptor.Size {
		view.CacheReason = "cache_descriptor_size_mismatch"
	} else if inspection.Present {
		view.CacheState = "complete"
		view.Reason = "awaiting_internal_transfer"
	} else if !inspection.Unknown && (intentErr == nil || errors.Is(intentErr, storage.ErrNotFound)) {
		view.CacheState = "missing"
		view.Reason = "waiting_external_payload"
		view.NextAction = "client_pull_required"
	}
	node, err := s.records.GetGraphNode(ctx, task.ID, edge.ChildRole, edge.ChildDigest)
	if err != nil {
		view.ReadErrors["graph"] = diagnosticReason(err)
	} else if node.ContentGeneration != nil && s.gate != nil && node.GCEpoch == s.gate.State().Epoch {
		generation, err := s.observeGeneration(ctx, task.Authority, task.Repository, edge.ChildDigest)
		if err != nil {
			view.ReadErrors["generation"] = diagnosticReason(err)
		} else if !generation.Tombstone && generation.Generation == *node.ContentGeneration && (node.LocalState == "verified_present" || node.LocalState == "anchored") {
			observation, err := s.records.FindLocalObservation(ctx, task.Authority, task.Repository, edge.ChildRole, edge.ChildDigest, node.ActualSize, *node.ContentGeneration, node.GCEpoch)
			if err == nil {
				view.RegistryState = "verified"
				view.CheckedAt = observation.ObservedAt
				view.Reason = "available"
				view.NextAction = "none"
			} else {
				view.ReadErrors["observation"] = diagnosticReason(err)
			}
		}
	}
	if intent != nil {
		view.IntentCacheVersion = intent.CacheVersion
		s.enrichIntentDependency(ctx, *intent, &view)
	}
	return view
}

func (s *Service) inspectDependencyCache(ctx context.Context, task tasks.Task, edge tasks.DependencyEdge) (artifact.Inspection, *tasks.BlobIntent, error, error) {
	if edge.ChildRole != tasks.GraphBlob {
		inspection, err := s.cache.Inspect(ctx, task.Authority, task.Repository, edge.ChildDigest, edge.Descriptor.MediaType)
		return inspection, nil, nil, err
	}
	intent, intentErr := s.records.GetBlobIntentByIdentity(ctx, tasks.BlobIdentity{Authority: task.Authority, Repository: task.Repository, Digest: edge.ChildDigest})
	preferred := edge.Descriptor.MediaType
	var current *tasks.BlobIntent
	if intentErr == nil {
		current = &intent
		if intent.MediaType != "" {
			preferred = intent.MediaType
		}
	}
	inspection, err := s.cache.InspectAny(ctx, task.Authority, task.Repository, edge.ChildDigest, preferred)
	return inspection, current, intentErr, err
}

func intentNextAction(state tasks.BlobIntentState) string {
	switch state {
	case tasks.IntentWaitingCache:
		return "client_pull_required"
	case tasks.IntentWaitingQuota:
		return "wait_capacity"
	case tasks.IntentRetryWait:
		return "wait_automatic_retry"
	case tasks.IntentFailed, tasks.IntentCancelled:
		return "management_action_required"
	default:
		return "wait_internal_work"
	}
}
