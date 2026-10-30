package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"strconv"
	"strings"
)

type imageDependencyPage struct {
	Snapshot        inventory.Snapshot `json:"snapshot"`
	Items           []dependencyView   `json:"items"`
	NextToken       string             `json:"nextToken,omitempty"`
	DependencyScope string             `json:"dependencyScope"`
	DirectEdgesURL  string             `json:"directEdgesURL,omitempty"`
}

func (s *Service) serveImageDependencies(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	clone := request.Clone(ctx)
	urlCopy := *request.URL
	clone.URL = &urlCopy
	values := clone.URL.Query()
	limit := 32
	if raw := values.Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 32 {
			writeInventoryError(writer, inventory.ErrInvalid)
			return
		}
	}
	values.Set("limit", strconv.Itoa(limit))
	clone.URL.RawQuery = values.Encode()
	clone.URL.Path = strings.TrimSuffix(clone.URL.Path, "/dependencies") + "/objects"
	query, err := parseInventoryQuery(clone)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	page, err := s.inventory.Objects(ctx, query, s.cursorKey, s.inventoryConfig().StaleAfter)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	image, err := s.inventory.Image(ctx, page.Snapshot, query.ImageID)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	result := imageDependencyPage{Snapshot: page.Snapshot, Items: []dependencyView{}, NextToken: page.NextToken, DependencyScope: "captured transitive objects and parent edges; per-object parent edge scope and truncation apply"}
	if len(image.Solidification.Tasks) > 0 {
		result.DirectEdgesURL = "/api/v1/tasks/" + image.Solidification.Tasks[0].ID + "/dependencies"
	}
	for _, object := range page.Items {
		view := dependencyView{Digest: object.Digest, Role: object.Role, DeclaredBytes: object.Size, CacheState: object.CacheState, RegistryState: "unknown", IntentID: object.IntentID, CheckedAt: object.CheckedAt, EvidenceSource: "immutable inventory snapshot; linked intent/work observations are live", Reason: "waiting_external_payload", NextAction: "client_pull_required", ReadErrors: map[string]string{}}
		view.ParentDigests, view.ParentEdgesScope, view.ParentEdgesTruncated = object.ParentDigests, object.ParentEdgesScope, object.ParentEdgesTruncated
		if view.ParentEdgesScope == "" {
			view.ParentEdgesScope = "unknown_legacy_snapshot"
		}
		if len(view.ParentDigests) == 1 {
			view.ParentDigest = view.ParentDigests[0]
		}
		if object.CacheState == "complete" {
			view.Reason, view.NextAction = "cached_waiting_internal_work", "wait_internal_work"
		}
		if object.CacheState == "unknown" {
			view.Reason, view.NextAction = "evidence_unknown", "await_verification"
		}
		if object.DistributionVerified {
			view.RegistryState, view.Reason, view.NextAction = "verified", "available", "none"
		}
		if object.Platform != nil {
			view.Platform, _ = json.Marshal(object.Platform)
		}
		if len(object.TaskIDs) > 0 {
			view.TaskID = object.TaskIDs[0]
		}
		if object.Role != "manifest" {
			intent, readErr := s.records.GetBlobIntentByIdentity(ctx, tasks.BlobIdentity{Authority: image.Authority, Repository: image.Repository, Digest: object.Digest})
			if readErr == nil {
				s.enrichIntentDependency(ctx, intent, &view)
			} else if !errors.Is(readErr, storage.ErrNotFound) {
				view.ReadErrors["intent"] = diagnosticReason(readErr)
			}
		}
		result.Items = append(result.Items, view)
	}
	writeJSON(writer, http.StatusOK, result)
}
