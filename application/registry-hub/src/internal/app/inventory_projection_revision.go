package app

import (
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"strings"
	"time"
)

type inventoryProjectionRevision struct {
	SourceRevision    string    `json:"sourceRevision"`
	ProjectedRevision string    `json:"projectedRevision"`
	SourceObservedAt  time.Time `json:"sourceObservedAt"`
	ProjectedAt       time.Time `json:"projectedAt"`
	WorkID            string    `json:"workID"`
}

type inventoryProjectionView struct {
	inventoryProjectionRevision
	CapturedRevision string                          `json:"capturedRevision"`
	SnapshotID       string                          `json:"snapshotID"`
	CapturedAt       time.Time                       `json:"capturedAt"`
	State            string                          `json:"state"`
	Coverage         string                          `json:"coverage"`
	SourceScans      map[string]backgroundScanStatus `json:"sourceScans"`
	ReadErrors       map[string]string               `json:"readErrors,omitempty"`
	Execution        executionProjectionView         `json:"execution"`
}

func projectionRevisionKey(id string) string {
	return inventory.Prefix + "projection-revisions/" + id + ".json"
}

func (s *Service) recordProjectionRevision(ctx context.Context, job *workqueue.Job, source inventoryManifestWork, done bool) error {
	ids := []string{inventory.ID("platform", source.Identity.Authority, source.Identity.Repository, source.Digest)}
	if source.Identity.Reference != "" {
		ids = append(ids, source.Identity.ID())
	}
	for _, id := range ids {
		if err := inventory.Update(ctx, s.store, projectionRevisionKey(id), func(value *inventoryProjectionRevision) error {
			revision := workqueue.SourceRevision(*job)
			// Completion acknowledges only its observed source; opaque revisions have no time ordering.
			if done && value.SourceRevision != "" && value.SourceRevision != revision {
				return nil
			}
			value.SourceRevision, value.SourceObservedAt, value.WorkID = revision, job.CreatedAt, job.ID
			if done {
				value.ProjectedRevision, value.ProjectedAt = value.SourceRevision, time.Now().UTC()
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) captureProjectionRevision(ctx context.Context, image *inventory.Image) error {
	value, _, err := inventory.Read[inventoryProjectionRevision](ctx, s.store, projectionRevisionKey(image.ID))
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if value.SourceRevision == value.ProjectedRevision {
		image.CapturedProjectionRevision = value.ProjectedRevision
	}
	return nil
}

func (s *Service) validateCapturedProjection(ctx context.Context, image *inventory.Image) error {
	if image.CapturedProjectionRevision == "" {
		return nil
	}
	value, _, err := inventory.Read[inventoryProjectionRevision](ctx, s.store, projectionRevisionKey(image.ID))
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if err != nil || value.SourceRevision != image.CapturedProjectionRevision || value.ProjectedRevision != image.CapturedProjectionRevision {
		image.CapturedProjectionRevision = ""
		image.Evidence.Reasons = appendReason(image.Evidence.Reasons, "projection_changed_during_capture")
	}
	return nil
}

func (s *Service) serveImageProjection(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/api/v1/images/"), "/projection")
	if !inventory.ValidID(id) || !onlyQueryKeys(request.URL.Query(), "snapshotID") {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	snapshot, err := s.inventory.Snapshot(ctx, request.URL.Query().Get("snapshotID"), s.inventoryConfig().StaleAfter)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	image, err := s.inventory.Image(ctx, snapshot, id)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	view := inventoryProjectionView{CapturedRevision: image.CapturedProjectionRevision, SnapshotID: snapshot.SnapshotID, CapturedAt: snapshot.ObservedAt, State: "unknown", Coverage: "non-atomic source sweeps; unseen source commits and enqueue failures remain possible", SourceScans: map[string]backgroundScanStatus{}, ReadErrors: map[string]string{}}
	view.inventoryProjectionRevision, _, err = inventory.Read[inventoryProjectionRevision](ctx, s.store, projectionRevisionKey(id))
	if err != nil {
		view.ReadErrors["revision"] = diagnosticReason(err)
	} else if view.SourceRevision != view.ProjectedRevision {
		view.State = "projection_pending"
	} else if view.ProjectedRevision != "" && view.ProjectedRevision != view.CapturedRevision {
		view.State = "snapshot_pending"
	} else if view.CapturedRevision != "" {
		view.State = "captured_as_observed"
	}
	for _, kind := range []string{"source_tasks", "source_cache"} {
		value, _, err := inventory.Read[backgroundScanStatus](ctx, s.store, "background/v1/scans/"+kind+".json")
		if err != nil {
			view.ReadErrors[kind] = diagnosticReason(err)
		}
		sanitizeScanStatus(&value)
		value.Stale = value.LastSuccessAt.IsZero() || time.Since(value.LastSuccessAt) > 90*time.Second
		view.SourceScans[kind] = value
	}
	view.Execution = s.imageExecutionProjection(ctx, image)
	writeJSON(writer, http.StatusOK, view)
}
