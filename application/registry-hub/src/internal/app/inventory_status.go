package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

type inventoryBuildStatus struct {
	Background               *backgroundStatus      `json:"background,omitempty"`
	Building                 bool                   `json:"building"`
	ScanStartedAt            time.Time              `json:"scanStartedAt,omitempty"`
	BuildStartedAt           time.Time              `json:"buildStartedAt,omitempty"`
	RefreshElapsedSeconds    float64                `json:"refreshElapsedSeconds"`
	BuildElapsedSeconds      float64                `json:"buildElapsedSeconds"`
	LastPublishedAt          time.Time              `json:"lastPublishedAt,omitempty"`
	LastCompletePublishedAt  time.Time              `json:"lastCompletePublishedAt,omitempty"`
	LastBuildSeconds         float64                `json:"lastBuildSeconds"`
	LastRefreshSeconds       float64                `json:"lastRefreshSeconds"`
	SnapshotExpiresInSeconds *float64               `json:"snapshotExpiresInSeconds"`
	LocalDirtyHint           bool                   `json:"localDirtyHint"`
	SourceCoverage           string                 `json:"sourceCoverage"`
	ReadErrors               map[string]string      `json:"readErrors,omitempty"`
	Fence                    uint64                 `json:"fence"`
	ProgressAt               time.Time              `json:"progressAt"`
	Steps                    uint64                 `json:"steps"`
	Batch                    inventoryBatchProgress `json:"batch"`
	SourcePhase              int                    `json:"sourcePhase"`
	SourceToken              string                 `json:"sourceToken,omitempty"`
	SourceOffset             int                    `json:"sourceOffset"`
	TaskID                   string                 `json:"taskID,omitempty"`
	GraphPhase               int                    `json:"graphPhase"`
	GraphStage               string                 `json:"graphStage,omitempty"`
	GraphToken               string                 `json:"graphToken,omitempty"`
	GraphOffset              int                    `json:"graphOffset"`
	ParentDigest             string                 `json:"parentDigest,omitempty"`
	ChildOffset              int                    `json:"childOffset"`
	NodePhase                int                    `json:"nodePhase"`
	GraphRestarts            uint64                 `json:"graphRestarts"`
	SnapshotID               string                 `json:"snapshotID,omitempty"`
	BuildPhase               int                    `json:"buildPhase"`
	ImageID                  string                 `json:"imageID,omitempty"`
	ImagePhase               int                    `json:"imagePhase"`
	ObjectOffset             int                    `json:"objectOffset"`
	AuditOffset              int64                  `json:"auditOffset"`
	CapturePhase             int                    `json:"capturePhase"`
	CaptureOffset            int                    `json:"captureOffset"`
	ClosureDepth             int                    `json:"closureDepth"`
	ErrorAt                  time.Time              `json:"errorAt,omitempty"`
	ErrorClass               string                 `json:"errorClass,omitempty"`
	NextAttemptAt            time.Time              `json:"nextAttemptAt,omitempty"`
	Incremental              *inventoryDeltaStatus  `json:"incremental,omitempty"`
	NextReconcileAt          time.Time              `json:"nextReconcileAt,omitempty"`
	FragmentValidationOffset int                    `json:"fragmentValidationOffset"`
	ImageConcurrency         int                    `json:"imageConcurrency"`
	ImagesInProgress         int                    `json:"imagesInProgress"`
}

type inventoryBuildError struct {
	ErrorAt       time.Time `json:"errorAt"`
	ErrorClass    string    `json:"errorClass"`
	NextAttemptAt time.Time `json:"nextAttemptAt"`
}

func (s *Service) inventoryStatus(ctx context.Context) (inventoryBuildStatus, error) {
	state, _, err := inventory.Read[inventoryCheckpoint](ctx, s.store, inventory.Prefix+"checkpoint.json")
	status := inventoryBuildStatus{Fence: state.Fence, ProgressAt: state.ProgressAt, Steps: state.Steps, SourcePhase: state.Scan.Phase, SourceToken: state.Scan.Token, BuildPhase: state.BuildPhase}
	status.ReadErrors = map[string]string{}
	status.ScanStartedAt, status.LastPublishedAt, status.LastCompletePublishedAt = state.ScanStartedAt, state.LastPublishedAt, state.LastCompleteAt
	status.LastBuildSeconds, status.LastRefreshSeconds = state.LastBuildSeconds, state.LastRefreshSeconds
	status.LocalDirtyHint = s.inventoryDirty.Load()
	status.SourceCoverage = "durable change markers with bounded fanout; non-atomic periodic reconciliation recovers missed markers and legacy sources; localDirtyHint is process-local"
	status.NextReconcileAt = state.NextReconcileAt
	status.ImageConcurrency = 1
	if s.inventoryConfig().BatchSize >= 4 {
		status.ImageConcurrency = 4
	}
	if state.ImageBuild != nil {
		status.ImagesInProgress = 1 + len(state.ParallelImages) + len(state.DeferredImages)
	}
	if delta := state.Delta; delta != nil {
		status.Building = true
		status.Incremental = &inventoryDeltaStatus{Phase: delta.Phase, BufferedChanges: len(delta.Changes), SourceOffset: delta.Offset, JournalOffset: delta.Cursor.Offset, FanoutOffset: delta.Fanout.Offset}
		if delta.Source != nil {
			status.TaskID, status.GraphPhase = delta.Source.Task.ID, delta.Source.Phase
		}
	}
	if !state.ScanStartedAt.IsZero() {
		status.RefreshElapsedSeconds = max(0, time.Since(state.ScanStartedAt).Seconds())
	}
	if err != nil {
		status.ReadErrors["checkpoint"] = diagnosticReason(err)
	}
	status.Batch = state.Batch
	if state.Scan.Page != nil {
		status.SourceToken, status.SourceOffset = state.Scan.Page.Token, state.Scan.Page.Offset
	}
	if root := state.Scan.Root; root != nil {
		status.TaskID, status.GraphPhase = root.Task.ID, root.Phase
		status.GraphRestarts = root.Restarts
		status.GraphToken, status.GraphOffset = root.Cursor.Token, root.Cursor.Offset
		stages := []string{"register", "index_edges", "project_nodes", "validate", "cleanup"}
		if root.Phase >= 0 && root.Phase < len(stages) {
			status.GraphStage = stages[root.Phase]
		}
		if root.Phase == 1 && !root.Nodes.Done {
			status.GraphStage = "capture_nodes"
			status.GraphToken, status.GraphOffset = root.Nodes.Token, root.Nodes.Offset
		}
		if root.Node != nil {
			status.ParentDigest, status.ChildOffset = root.Node.Node.Digest, len(root.Node.Content.Children)
			status.NodePhase = root.Node.Phase
		}
	}
	if state.Build != nil {
		status.Building = true
		status.SnapshotID = state.Build.SnapshotID
		status.BuildStartedAt = state.Build.BuildStartedAt
		status.BuildElapsedSeconds = max(0, time.Since(state.Build.BuildStartedAt).Seconds())
	}
	if image := state.ImageBuild; image != nil {
		status.ImageID, status.ImagePhase, status.ObjectOffset = image.Image.ID, image.Phase, image.Offset
		status.CapturePhase, status.CaptureOffset = image.Capture.Phase, image.Capture.Offset
		status.ClosureDepth = len(image.Closure.Frames)
		if image.Reuse != nil {
			status.FragmentValidationOffset = image.Reuse.Offset
		}
		if image.Audit != nil {
			status.AuditOffset = image.Audit.Offset
		}
	}
	failure, _, err := inventory.Read[inventoryBuildError](ctx, s.store, inventory.Prefix+"error.json")
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		status.ReadErrors["build_error"] = diagnosticReason(err)
	}
	if failure.ErrorAt.After(status.ProgressAt) {
		status.ErrorAt, status.ErrorClass, status.NextAttemptAt = failure.ErrorAt, failure.ErrorClass, failure.NextAttemptAt
	}
	background := s.backgroundStatus(ctx)
	status.Background = &background
	if background.Snapshot != nil {
		remaining := time.Until(background.Snapshot.ExpiresAt).Seconds()
		status.SnapshotExpiresInSeconds = &remaining
	}
	if status.SourceToken != "" {
		status.SourceToken = inventory.ID("checkpoint-token", status.SourceToken)
	}
	if status.GraphToken != "" {
		status.GraphToken = inventory.ID("checkpoint-token", status.GraphToken)
	}
	return status, nil
}

type inventoryDeltaStatus struct {
	Phase           int `json:"phase"`
	BufferedChanges int `json:"bufferedChanges"`
	SourceOffset    int `json:"sourceOffset"`
	JournalOffset   int `json:"journalOffset"`
	FanoutOffset    int `json:"fanoutOffset"`
}

func (s *Service) inventoryErrorStatus(ctx context.Context, cause error, delay time.Duration) {
	class := "storage"
	switch {
	case errors.Is(cause, context.DeadlineExceeded), errors.Is(cause, storage.ErrTimeout):
		class = "timeout"
	case errors.Is(cause, storage.ErrCorruptObject):
		class = "corrupt_object"
	case errors.Is(cause, storage.ErrLeaseLost), errors.Is(cause, inventory.ErrSuperseded), errors.Is(cause, context.Canceled):
		class = "lease_lost"
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	now := time.Now().UTC()
	_ = inventory.Update(ctx, s.store, inventory.Prefix+"error.json", func(value *inventoryBuildError) error {
		*value = inventoryBuildError{ErrorAt: now, ErrorClass: class, NextAttemptAt: now.Add(delay)}
		return nil
	})
}
