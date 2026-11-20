package app

import (
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

type storageReportStatus struct {
	SnapshotID  string    `json:"snapshotID"`
	CompletedAt time.Time `json:"completedAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Freshness   string    `json:"freshness"`
	Available   bool      `json:"available"`
}

type storageCollectionStatus struct {
	Cost                 storageCollectionCost           `json:"cost"`
	BudgetYields         uint64                          `json:"budgetYields"`
	LastYieldReason      string                          `json:"lastYieldReason,omitempty"`
	ObservedAt           time.Time                       `json:"observedAt"`
	CheckpointAt         time.Time                       `json:"checkpointAt"`
	EvidenceSource       string                          `json:"evidenceSource"`
	BuildID              string                          `json:"buildID"`
	Phase                string                          `json:"phase"`
	StartedAt            time.Time                       `json:"startedAt"`
	ProgressAt           time.Time                       `json:"progressAt"`
	LastCompletedAt      time.Time                       `json:"lastCompletedAt"`
	NextScanAt           time.Time                       `json:"nextScanAt"`
	NextRetryAt          time.Time                       `json:"nextRetryAt"`
	ElapsedSeconds       float64                         `json:"elapsedSeconds"`
	Domains              map[string]storageDomainSummary `json:"domains"`
	EntriesExamined      *int64                          `json:"entriesExamined"`
	ObjectsCommitted     *int64                          `json:"objectsCommitted"`
	TotalObjects         *int64                          `json:"totalObjects"`
	TotalBytes           *int64                          `json:"totalBytes"`
	PercentComplete      *float64                        `json:"percentComplete"`
	CounterReason        string                          `json:"counterReason,omitempty"`
	OutputPages          int                             `json:"outputPages"`
	AttributionObjects   *int64                          `json:"attributionObjects"`
	UsagePhase           string                          `json:"usagePhase"`
	UsageLevel           int                             `json:"usageLevel"`
	UsageCompletedPairs  int                             `json:"usageCompletedPairs"`
	UsageBatchPending    bool                            `json:"usageBatchPending"`
	UsageBatchRun        *storageUsageRun                `json:"usageBatchRun,omitempty"`
	AutomaticRestarts    int                             `json:"automaticRestarts"`
	RecoveredFromBuildID string                          `json:"recoveredFromBuildID,omitempty"`
	RecoveryReason       string                          `json:"recoveryReason,omitempty"`
	ExecutionUnits       uint64                          `json:"executionUnits"`
	PreviousReport       *storageReportStatus            `json:"previousReport"`
	ErrorClass           string                          `json:"errorClass,omitempty"`
	ErrorAt              time.Time                       `json:"errorAt"`
	RecoveredAt          time.Time                       `json:"recoveredAt"`
	Backoff              bool                            `json:"backoff"`
	Worker               *workerStatus                   `json:"worker,omitempty"`
	ReadErrors           map[string]string               `json:"readErrors,omitempty"`
}

func (s *Service) serveStorageStatus(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	if len(request.URL.Query()) != 0 {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	status, err := s.storageCollectionStatus(ctx)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, status)
}

func (s *Service) storageCollectionStatus(ctx context.Context) (storageCollectionStatus, error) {
	now := time.Now().UTC()
	state, _, stateErr := inventory.Read[storageCollection](ctx, s.store, storageInventoryPrefix+"checkpoint.json")
	report, _, reportErr := inventory.Read[storageSnapshot](ctx, s.store, storageInventoryPrefix+"current.json")
	maintenance, _, maintenanceErr := inventory.Read[maintenanceCheckpoint](ctx, s.store, maintenanceCheckpointKey("storage_inventory"))
	status := storageCollectionStatus{ObservedAt: now, Phase: "not_observed", EvidenceSource: "persisted_collection_checkpoint; persisted_current_report; persisted_maintenance_checkpoint; process_worker", Domains: map[string]storageDomainSummary{}, ReadErrors: map[string]string{}}
	for source, err := range map[string]error{"collection": stateErr, "report": reportErr, "maintenance": maintenanceErr} {
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			status.ReadErrors[source] = diagnosticReason(err)
		}
	}
	if stateErr == nil {
		status.applyCollection(state, now)
	}
	if reportErr == nil && report.SnapshotID != "" {
		status.PreviousReport = &storageReportStatus{SnapshotID: report.SnapshotID, CompletedAt: report.CompletedAt, ExpiresAt: report.ExpiresAt, Freshness: "expired", Available: report.ExpiresAt.After(now)}
		if status.PreviousReport.Available {
			status.PreviousReport.Freshness = "fresh"
		}
		status.LastCompletedAt = report.CompletedAt
		if status.Phase == "not_observed" {
			status.Phase = "idle"
		}
	}
	if maintenanceErr == nil {
		scan := maintenance.Status
		status.ExecutionUnits = scan.Processed
		status.ErrorClass, status.ErrorAt, status.RecoveredAt = scan.ErrorClass, scan.ErrorAt, scan.RecoveredAt
		if scan.ErrorRecovered && status.RecoveredAt.IsZero() && scan.LastSuccessAt.After(scan.ErrorAt) {
			status.RecoveredAt = scan.LastSuccessAt
		}
		status.NextRetryAt = scan.NextAttemptAt
		status.Backoff = scan.NextAttemptAt.After(now)
		if status.CheckpointAt.IsZero() {
			status.CheckpointAt = scan.CheckpointAt
		}
	}
	s.workerMu.Lock()
	if worker, ok := s.workerStates["storage_inventory"]; ok {
		status.Worker = &worker
	}
	s.workerMu.Unlock()
	if len(status.ReadErrors) > 0 && status.BuildID == "" && status.PreviousReport == nil && (maintenanceErr != nil || maintenance.Status.StartedAt.IsZero()) {
		return status, storage.ErrUnavailable
	}
	if stateErr != nil && !errors.Is(stateErr, storage.ErrNotFound) && status.BuildID == "" {
		status.Phase = "unknown"
	}
	return status, nil
}

func (status *storageCollectionStatus) applyCollection(state storageCollection, now time.Time) {
	status.Cost, status.BudgetYields, status.LastYieldReason = state.Snapshot.Cost, state.BudgetYields, state.LastYieldReason
	status.BuildID, status.Phase = state.Snapshot.SnapshotID, storageCollectionPhase(state)
	status.CheckpointAt, status.ProgressAt, status.StartedAt = state.CheckpointAt, state.ProgressAt, state.Snapshot.StartedAt
	status.LastCompletedAt, status.NextScanAt = state.LastCompletedAt, state.NextScanAt
	status.EntriesExamined, status.AttributionObjects = state.EntriesExamined, state.AttributionObjects
	status.OutputPages = state.Snapshot.Pages
	status.UsagePhase, status.UsageLevel, status.UsageCompletedPairs = state.UsageMerge.Phase, state.UsageMerge.Level, state.UsageMerge.Pair
	status.UsageBatchPending, status.AutomaticRestarts = state.UsagePending != nil, state.AutomaticRestarts
	status.RecoveredFromBuildID, status.RecoveryReason = state.RecoveredFromBuildID, state.RecoveryReason
	if state.UsagePending != nil {
		run := state.UsagePending.Run
		status.UsageBatchRun = &run
	}
	if !status.StartedAt.IsZero() {
		status.ElapsedSeconds = max(0, now.Sub(status.StartedAt).Seconds())
	}
	committed := int64(0)
	for index, domain := range []string{"state", "registry"} {
		summary, ok := state.Snapshot.Domains[domain]
		if !ok {
			continue
		}
		summary.Categories = nil
		if summary.State == "collecting" && (state.AttributionPhase < 3 || index > state.Domain) {
			summary.State = "waiting"
		}
		if summary.Objects != nil {
			committed += *summary.Objects
		}
		status.Domains[domain] = summary
	}
	if status.BuildID != "" {
		status.ObjectsCommitted = &committed
	}
	if state.EntriesExamined == nil && status.BuildID != "" {
		status.CounterReason = "legacy_checkpoint; examined entries unknown; committed objects derived from domains"
	}
}

func storageCollectionPhase(state storageCollection) string {
	if state.Snapshot.SnapshotID == "" {
		if !state.NextScanAt.IsZero() {
			return "idle"
		}
		return "not_observed"
	}
	if state.AttributionPhase < 3 {
		return "attribution"
	}
	if state.Domain == 0 {
		return "collect_state"
	}
	if state.Domain == 1 {
		return "collect_registry"
	}
	switch state.UsageMerge.Phase {
	case "", "reduce":
		return "usage_reduce"
	case "seed_sort", "sort":
		return "usage_sort"
	default:
		return "publishing"
	}
}
