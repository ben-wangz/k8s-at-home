package app

import (
	"context"
	"net/http"
	"registry-hub/internal/buildinfo"
	"registry-hub/internal/inventory"
	"registry-hub/internal/tasks"
	"runtime/debug"
	"slices"
	"strings"
	"time"
)

type workerView struct {
	workerStatus
	InstanceID              string                `json:"instanceID"`
	Enabled                 bool                  `json:"enabled"`
	Implemented             bool                  `json:"implemented"`
	Scan                    *backgroundScanStatus `json:"scan,omitempty"`
	Probe                   *healthProbeStatus    `json:"probe,omitempty"`
	ReadReason              string                `json:"readReason,omitempty"`
	Solidification          *tasks.RuntimeStatus  `json:"solidification,omitempty"`
	ScheduleMode            string                `json:"scheduleMode"`
	NextScheduleAt          time.Time             `json:"nextScheduleAt"`
	SharedConcurrency       int                   `json:"sharedConcurrency"`
	AdmissionBudgetSeconds  float64               `json:"admissionBudgetSeconds"`
	CheckpointBudgetSeconds float64               `json:"checkpointBudgetSeconds"`
	LeaseOwner              string                `json:"leaseOwner,omitempty"`
	LeaseFence              uint64                `json:"leaseFence,omitempty"`
}

type workerList struct {
	Items      []workerView `json:"items"`
	InstanceID string       `json:"instanceID"`
	ObservedAt time.Time    `json:"observedAt"`
}

type capacityStatus struct {
	ProtectedCacheReservedBytes int64     `json:"protectedCacheReservedBytes"`
	ProtectedCacheLimitBytes    int64     `json:"protectedCacheLimitBytes"`
	UnanchoredReservedBytes     int64     `json:"unanchoredReservedBytes"`
	UnanchoredLegacyBytes       int64     `json:"unanchoredLegacyBytes"`
	UnanchoredLimitBytes        int64     `json:"unanchoredLimitBytes"`
	GCState                     string    `json:"gcState"`
	GCEpoch                     uint64    `json:"gcEpoch"`
	RecoveryBarrier             bool      `json:"recoveryBarrier"`
	ObservedAt                  time.Time `json:"observedAt"`
	EvidenceSource              string    `json:"evidenceSource"`
	GCStatusURL                 string    `json:"gcStatusURL"`
}

type systemStatus struct {
	Version                 string               `json:"version"`
	Revision                string               `json:"revision,omitempty"`
	StartedAt               time.Time            `json:"startedAt"`
	InstanceID              string               `json:"instanceID"`
	Ready                   bool                 `json:"ready"`
	Inventory               inventoryBuildStatus `json:"inventory"`
	Workers                 []workerView         `json:"workers"`
	Capabilities            map[string]string    `json:"capabilities"`
	SourceCoverage          string               `json:"sourceCoverage"`
	DiagnosticEventsDropped uint64               `json:"diagnosticEventsDropped"`
	Capacity                *capacityStatus      `json:"capacity"`
	ReferrerRepairsInFlight int                  `json:"referrerRepairsInFlight"`
	ReferrerRepairPending   bool                 `json:"referrerRepairPending"`
	ReferrerStatusURL       string               `json:"referrerStatusURL"`
}

func (s *Service) systemStatus(ctx context.Context) (systemStatus, error) {
	build, err := s.inventoryStatus(ctx)
	status := systemStatus{Version: buildinfo.Version, StartedAt: s.startedAt, InstanceID: s.instanceID, Ready: s.ready.Load(), Inventory: build, SourceCoverage: "non_atomic_sweeps; queue emptiness does not establish source coverage", Capabilities: map[string]string{}, DiagnosticEventsDropped: s.diagnosticEventsDropped.Load()}
	status.Capacity = s.diagnosticCapacity()
	if status.Capacity != nil {
		status.Capabilities["gc"] = status.Capacity.GCState
		if status.Capacity.RecoveryBarrier {
			status.Capabilities["gc"] = "recovery_barrier"
		}
		status.Capabilities["protected_cache_capacity"] = "available"
		if status.Capacity.ProtectedCacheLimitBytes > 0 && status.Capacity.ProtectedCacheReservedBytes >= status.Capacity.ProtectedCacheLimitBytes {
			status.Capabilities["protected_cache_capacity"] = "capacity_blocked"
		}
		status.Capabilities["unanchored_capacity"] = "available"
		if status.Capacity.UnanchoredLimitBytes > 0 && status.Capacity.UnanchoredReservedBytes >= status.Capacity.UnanchoredLimitBytes {
			status.Capabilities["unanchored_capacity"] = "capacity_blocked"
		}
	}
	s.solidificationMu.Lock()
	status.ReferrerRepairsInFlight, status.ReferrerRepairPending = s.referrerRepairs, s.repairPending
	s.solidificationMu.Unlock()
	status.ReferrerStatusURL = "/api/v1/referrers/reconcile"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				status.Revision = setting.Value
			}
		}
	}
	if build.Background != nil {
		status.Workers = s.workerViews(*build.Background)
		status.Capabilities["inventory"] = "available"
		if build.Background.Snapshot == nil {
			status.Capabilities["inventory"] = "not_observed"
		} else if build.Background.Snapshot.Freshness.State != "fresh" {
			status.Capabilities["inventory"] = build.Background.Snapshot.Freshness.State
		}
		status.Capabilities["download_capacity"] = "unknown"
		if reserved := build.Background.DownloadReservedBytes; reserved != nil {
			status.Capabilities["download_capacity"] = "available"
			if *reserved >= s.cfg.Limits.DownloadMaxBytes && s.cfg.Limits.DownloadMaxBytes > 0 {
				status.Capabilities["download_capacity"] = "capacity_blocked"
			}
		}
	}
	return status, err
}

func (s *Service) workerViews(background backgroundStatus) []workerView {
	s.workerMu.Lock()
	states := make(map[string]workerStatus, len(s.workerStates))
	for id, state := range s.workerStates {
		states[id] = state
	}
	s.workerMu.Unlock()
	ids := append(append([]string{}, backgroundKinds...), backgroundScanKinds...)
	for id := range states {
		ids = append(ids, id)
	}
	ids = append(ids, "activation_events", "inventory_builder", "statistics_tasks", "statistics_cache", "statistics_intents", "statistics_protection", "storage_probe", "registry_probe", "storage_inventory", "index_repair", "index_maintenance", "solidification", "protection_recovery", "diagnostic_events")
	slices.Sort(ids)
	views := []workerView{}
	for _, id := range slices.Compact(ids) {
		state, exists := states[id]
		if !exists {
			state = workerStatus{ID: id, Scope: "process", State: "not_observed"}
		}
		view := workerView{workerStatus: state, InstanceID: s.instanceID, Enabled: true, Implemented: true}
		s.configureWorkerBudgets(&view)
		if slices.Contains(backgroundKinds, id) {
			view.ScheduleMode = "durable_queue"
			if queue, ok := background.Queues[id]; ok {
				view.Resumability, view.LeaseOwner, view.LeaseFence = queue.Resumability, queue.LeaseOwner, queue.LeaseFence
				if queue.NextAttemptAt != nil {
					view.NextScheduleAt = *queue.NextAttemptAt
				}
			}
		}
		if id == "activation_events" || id == "diagnostic_events" {
			view.ScheduleMode = "events"
		}
		if id == "index_maintenance" {
			view.Enabled, view.Implemented, view.State, view.ScheduleMode = false, false, "not_implemented", "none"
		}
		if scan, ok := background.Maintenance[id]; ok {
			view.Scan = &scan
			view.ReadReason = background.ReadErrors["maintenance/"+id]
		}
		if scan, ok := background.Statistics[strings.TrimPrefix(id, "statistics_")]; ok && strings.HasPrefix(id, "statistics_") {
			view.Scan = &scan
			view.ReadReason = background.ReadErrors["statistics/"+strings.TrimPrefix(id, "statistics_")]
		}
		if id == "storage_probe" || id == "registry_probe" {
			probe := background.Health[strings.TrimSuffix(id, "_probe")]
			view.Probe = &probe
			view.State = "periodic"
		}
		if id == "index_repair" {
			view.ScheduleMode = "startup_only"
			if view.State == "not_observed" {
				view.ReadReason = "startup_result_not_observed"
			}
		}
		if id == "solidification" && s.tasks != nil {
			runtime := s.tasks.Runtime()
			view.Solidification, view.LastHeartbeatAt = &runtime, runtime.LastHeartbeatAt
			view.State = "idle"
			if len(runtime.ActiveTasks) > 0 {
				view.State = "execution"
			}
		}
		if view.Scan != nil && !view.Scan.NextAttemptAt.IsZero() && view.Scan.NextAttemptAt.After(time.Now()) {
			view.State = "backoff"
		}
		if view.Scan != nil {
			if view.Scan.LastError != nil && (view.LastError == nil || !view.Scan.LastError.OccurredAt.Before(view.LastError.OccurredAt)) {
				view.LastError, view.RecoveredAt = view.Scan.LastError, view.Scan.RecoveredAt
			}
			view.LeaseOwner, view.LeaseFence = view.Scan.Owner, view.Scan.Fence
			view.NextScheduleAt = view.Scan.NextAttemptAt
			view.LastEntityProgressAt = view.Scan.LastEntityProgressAt
			view.Resumability = view.Scan.Resumability
			if view.NextScheduleAt.IsZero() {
				view.NextScheduleAt = view.Scan.NextScanAt
			}
		}
		views = append(views, view)
	}
	return views
}

func (s *Service) serveWorkers(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	if len(request.URL.Query()) != 0 {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	views := s.workerViews(s.backgroundStatus(ctx))
	if request.URL.Path == "/api/v1/workers" {
		writeJSON(writer, http.StatusOK, workerList{Items: views, InstanceID: s.instanceID, ObservedAt: time.Now().UTC()})
		return
	}
	id := strings.TrimPrefix(request.URL.Path, "/api/v1/workers/")
	for _, view := range views {
		if view.ID == id {
			writeJSON(writer, http.StatusOK, view)
			return
		}
	}
	writeAdminError(writer, http.StatusNotFound, "NAME_UNKNOWN", "worker does not exist")
}

func (s *Service) diagnosticCapacity() *capacityStatus {
	if s.gate == nil {
		return nil
	}
	state := s.gate.State()
	return &capacityStatus{ProtectedCacheReservedBytes: state.ProtectedCacheBytes, ProtectedCacheLimitBytes: protectedCacheLimit(s.cfg), UnanchoredReservedBytes: state.ReservedDistributionBytes, UnanchoredLegacyBytes: state.LegacyGrandfatheredBytes, UnanchoredLimitBytes: s.cfg.Solidification.MaxUnanchoredDistributionBytes, GCState: state.State, GCEpoch: state.Epoch, RecoveryBarrier: state.RecoveryBarrier, ObservedAt: time.Now().UTC(), EvidenceSource: "process_guarded_gate; reservations are not physical object bytes", GCStatusURL: "/api/v1/gc"}
}
