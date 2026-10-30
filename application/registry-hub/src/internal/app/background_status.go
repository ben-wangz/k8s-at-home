package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"sync"
	"time"
)

var backgroundKinds = []string{"inventory", "execution", "download_cleanup", "notify", "wake_root"}
var backgroundScanKinds = []string{"source_tasks", "source_cache", "source_downloads", "source_demands", "activation", "task_reconciliation", "quota_waiters", "cache_cleanup", "reader_cleanup", "upload_cleanup", "task_cleanup", "download_expiry", "receipt_cleanup", "download_quota", "storage_orphans", "storage_inventory", "history_tasks", "history_intents", "history_work", "history_receipts", "history_downloads", "history_uploads", "history_upload_receipts", "history_scans", "history_statistics", "history_maintenance", "history_workers", "diagnostic_retention", "retry_request_retention"}

type backgroundStatus struct {
	Queues                map[string]workqueue.Status     `json:"queues"`
	Statistics            map[string]backgroundScanStatus `json:"statistics"`
	Maintenance           map[string]backgroundScanStatus `json:"maintenance"`
	Health                map[string]healthProbeStatus    `json:"health"`
	Snapshot              *inventory.Snapshot             `json:"snapshot"`
	DownloadReservedBytes *int64                          `json:"downloadReservedBytes"`
	DownloadReservations  *int                            `json:"downloadReservations"`
	ReadErrors            map[string]string               `json:"readErrors,omitempty"`
}

func (s *Service) backgroundStatus(ctx context.Context) backgroundStatus {
	status := backgroundStatus{Queues: map[string]workqueue.Status{}, Statistics: map[string]backgroundScanStatus{}, Maintenance: map[string]backgroundScanStatus{}, ReadErrors: map[string]string{}, Health: map[string]healthProbeStatus{
		"storage":  probeStatus(s.storageHealthy.Load(), s.storageProbeAt.Load(), s.storageProbeSuccess.Load()),
		"registry": probeStatus(s.registryHealthy.Load(), s.registryProbeAt.Load(), s.registryProbeSuccess.Load()),
	}}
	var mu sync.Mutex
	var group sync.WaitGroup
	slots := make(chan struct{}, 4)
	read := func(name string, operation func() error) {
		group.Add(1)
		go func() {
			defer group.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				status.ReadErrors[name] = "timeout"
				mu.Unlock()
				return
			}
			defer func() { <-slots }()
			err := operation()
			if err != nil {
				mu.Lock()
				status.ReadErrors[name] = diagnosticReason(err)
				mu.Unlock()
			}
		}()
	}
	for _, kind := range backgroundKinds {
		read("queue/"+kind, func() error {
			value, _, err := inventory.Read[workqueue.Status](ctx, s.store, workqueue.Prefix+"status/"+kind+".json")
			value.Stale = value.ObservedAt.IsZero() || time.Since(value.ObservedAt) > 90*time.Second
			if value.ErrorEntity != "" {
				value.ErrorEntity = inventory.ID("diagnostic-entity", value.ErrorEntity)
			}
			if value.OldestAt != nil {
				age := max(int64(0), int64(time.Since(*value.OldestAt).Seconds()))
				value.OldestAgeSeconds = &age
			}
			mu.Lock()
			status.Queues[kind] = value
			mu.Unlock()
			return err
		})
	}
	for _, kind := range []string{"tasks", "cache", "intents", "protection"} {
		read("statistics/"+kind, func() error {
			value, _, err := inventory.Read[backgroundScanStatus](ctx, s.store, workqueue.Prefix+"statistics/"+kind+".json")
			value.Stale = value.LastSuccessAt.IsZero() || time.Since(value.LastSuccessAt) > 90*time.Second
			sanitizeScanStatus(&value)
			mu.Lock()
			status.Statistics[kind] = value
			mu.Unlock()
			return err
		})
	}
	for _, kind := range backgroundScanKinds {
		read("maintenance/"+kind, func() error {
			value, _, err := inventory.Read[backgroundScanStatus](ctx, s.store, workqueue.Prefix+"scans/"+kind+".json")
			lastSuccess := value.LastSuccessAt
			if lastSuccess.IsZero() && value.Complete {
				lastSuccess = value.ObservedAt
			}
			validity := 90 * time.Second
			if kind == "storage_inventory" {
				validity = 10 * time.Minute
			}
			value.Stale = lastSuccess.IsZero() || time.Since(lastSuccess) > validity
			sanitizeScanStatus(&value)
			mu.Lock()
			status.Maintenance[kind] = value
			mu.Unlock()
			return err
		})
	}
	read("snapshot", func() error {
		if s.inventory == nil {
			return inventory.ErrNotObserved
		}
		snapshot, err := s.inventory.Snapshot(ctx, "", s.inventoryConfig().StaleAfter)
		if !snapshot.ObservedAt.IsZero() {
			state, _, buildErr := inventory.Read[inventoryCheckpoint](ctx, s.store, inventory.Prefix+"checkpoint.json")
			snapshot.Building = buildErr == nil && state.Build != nil
			if buildErr != nil && !errors.Is(buildErr, storage.ErrNotFound) {
				mu.Lock()
				status.ReadErrors["snapshot_build"] = diagnosticReason(buildErr)
				mu.Unlock()
			}
			snapshot.Freshness = inventory.Freshness{State: "stale", AgeSeconds: max(0, int64(time.Since(snapshot.ObservedAt).Seconds()))}
			if err == nil && time.Since(snapshot.ObservedAt) <= s.inventoryConfig().StaleAfter {
				snapshot.Freshness.State = "fresh"
			}
			if errors.Is(err, inventory.ErrCurrentExpired) {
				snapshot.Freshness.State = "expired"
			}
			status.Snapshot = &snapshot
		}
		return err
	})
	read("download_quota", func() error {
		bytes, sessions, err := s.downloadManager().Usage(ctx)
		if err == nil {
			status.DownloadReservedBytes, status.DownloadReservations = &bytes, &sessions
		}
		return err
	})
	group.Wait()
	return status
}

func sanitizeScanStatus(value *backgroundScanStatus) {
	if value.ErrorEntity != "" {
		value.ErrorEntity = inventory.ID("diagnostic-entity", value.ErrorEntity)
	}
}
