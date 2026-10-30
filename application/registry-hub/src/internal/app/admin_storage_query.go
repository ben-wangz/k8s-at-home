package app

import (
	"context"
	"errors"
	"net/url"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"slices"
	"strings"
)

var storageObjectFilters = []string{"snapshotID", "domain", "category", "digest", "repository", "imageID", "platform", "associated", "workID", "taskID", "intentID", "transferID", "cleanupState", "protectionState", "reclaimState"}
var storageUsageFilters = []string{"snapshotID", "domain", "category", "repository", "dimension", "imageID", "platform", "sort"}

func storageQueryFields(path string) []string {
	switch path {
	case "/api/v1/storage/usage":
		return storageUsageFilters
	case "/api/v1/storage/objects", "/api/v1/storage/reclaimable":
		return storageObjectFilters
	default:
		return []string{"snapshotID"}
	}
}

func validStorageUsageQuery(values url.Values) bool {
	if dimension := values.Get("dimension"); dimension != "" && !slices.Contains([]string{"category", "repository", "image", "platform"}, dimension) {
		return false
	}
	return true
}

func validLegacyStorageUsageQuery(values url.Values) bool {
	selected := ""
	for _, filter := range []string{"category", "repository", "imageID", "platform"} {
		if values.Get(filter) == "" {
			continue
		}
		if selected != "" {
			return false
		}
		selected = filter
		if selected == "imageID" {
			selected = "image"
		}
	}
	return selected == "" || values.Get("dimension") == "" || values.Get("dimension") == selected
}

func matchesStorageUsage(usage storageUsage, values url.Values) bool {
	if usage.Mask != 0 {
		return matchesStorageUsageGroup(usage, values)
	}
	if (values.Get("domain") != "" && values.Get("domain") != usage.Domain) || (values.Get("dimension") != "" && values.Get("dimension") != usage.Dimension) {
		return false
	}
	for _, dimension := range []string{"category", "repository", "imageID", "platform"} {
		filter := values.Get(dimension)
		if filter == "" {
			continue
		}
		name := dimension
		if name == "imageID" {
			name = "image"
		}
		if usage.Dimension != name {
			return false
		}
		if name == "repository" {
			if usage.Name != filter && !strings.HasSuffix(usage.Name, "/"+filter) {
				return false
			}
		} else if usage.Name != filter {
			return false
		}
	}
	return true
}

func matchesStorageAssociation(object storageObject, values url.Values) bool {
	if values.Get("imageID") == object.ImageID && object.ImageID != "" && values.Get("platform") == "" && (values.Get("repository") == "" || values.Get("repository") == object.Repository) {
		return true
	}
	if values.Get("imageID") == "" && values.Get("platform") == "" && (values.Get("repository") == "" || object.Repository == values.Get("repository")) {
		return true
	}
	for _, association := range object.Associations {
		platform := ""
		if association.Platform != nil {
			platform = association.Platform.OS + "/" + association.Platform.Architecture
			if association.Platform.Variant != "" {
				platform += "/" + association.Platform.Variant
			}
		}
		if (values.Get("imageID") == "" || values.Get("imageID") == association.ImageID) && (values.Get("platform") == "" || values.Get("platform") == platform) && (values.Get("repository") == "" || values.Get("repository") == association.Repository) {
			return true
		}
	}
	return false
}

type storageSummaryView struct {
	storageSnapshot
	Freshness       string            `json:"freshness"`
	Building        bool              `json:"building"`
	BuildState      string            `json:"buildState"`
	BuildSnapshotID string            `json:"buildSnapshotID,omitempty"`
	ReadErrors      map[string]string `json:"readErrors,omitempty"`
	Capacity        *capacityStatus   `json:"capacity"`
}

func (s *Service) storageSummary(ctx context.Context, snapshot storageSnapshot) storageSummaryView {
	view := storageSummaryView{storageSnapshot: snapshot, Freshness: "fresh", BuildState: "observed", ReadErrors: map[string]string{}}
	view.Capacity = s.diagnosticCapacity()
	state, _, err := inventory.Read[storageCollection](ctx, s.store, storageInventoryPrefix+"checkpoint.json")
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		view.BuildState, view.ReadErrors["checkpoint"] = "unknown", diagnosticReason(err)
	} else {
		view.BuildSnapshotID, view.Building = state.Snapshot.SnapshotID, state.Snapshot.SnapshotID != ""
	}
	return view
}
