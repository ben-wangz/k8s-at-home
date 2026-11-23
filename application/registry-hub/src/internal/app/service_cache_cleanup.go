package app

import (
	"context"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"sync"
	"time"
)

func (s *Service) runCacheCleanup(ctx context.Context) {
	workers := []struct {
		kind, prefix string
		step         func(context.Context, storage.Entry) error
	}{
		{"cache_cleanup", "cache/objects/", func(ctx context.Context, entry storage.Entry) error {
			return s.cache.CleanupEntry(ctx, entry, time.Now().UTC())
		}},
		{"reader_cleanup", "cache/readers/", s.cache.CleanupReaderPinEntry},
		{"staging_reader_cleanup", "cache/staging-readers/", s.cache.CleanupReaderPinEntry},
		{"upload_cleanup", "uploads/sessions/", func(ctx context.Context, entry storage.Entry) error {
			return s.uploads.cleanupEntry(ctx, s.gate, entry)
		}},
		{"task_cleanup", "tasks/state/", func(ctx context.Context, entry storage.Entry) error {
			task, _, err := inventory.Read[tasks.Task](ctx, s.store, entry.Key)
			if err != nil {
				return err
			}
			if _, err := s.tasks.CancelIfExpired(ctx, task.ID, time.Now().UTC()); err != nil {
				return err
			}
			task, err = s.tasks.Get(ctx, task.ID)
			if err != nil {
				return err
			}
			if err := s.handoffDiagnosticOutbox(ctx, entry.Key, task.DiagnosticOutbox); err != nil {
				return err
			}
			return s.tasks.CleanupTaskStep(ctx, task.ID, 30*24*time.Hour)
		}},
		{"download_expiry", "downloads/v1/", func(ctx context.Context, entry storage.Entry) error {
			return s.downloadManager().CleanupExpiredStep(ctx, entry, time.Now().UTC(), s.cfg.Limits.DownloadPartialTTL, s.acquireDownloadCleanup)
		}},
	}
	receiptCleanupKind := ""
	if s.cfg.Diagnostics.FullWorkReceiptCleanupPreview {
		receiptCleanupKind = "receipt_cleanup_preview"
	} else if s.cfg.Diagnostics.FullWorkReceiptCleanupEnabled {
		receiptCleanupKind = "receipt_cleanup"
	}
	if receiptCleanupKind != "" {
		workers = append(workers, struct {
			kind, prefix string
			step         func(context.Context, storage.Entry) error
		}{receiptCleanupKind, "background/v1/receipts/", nil})
	}
	var group sync.WaitGroup
	for _, worker := range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			if worker.kind != "receipt_cleanup" && worker.kind != "receipt_cleanup_preview" {
				s.runSourceSweep(ctx, worker.kind, worker.prefix, worker.step)
				return
			}
			s.runSourceSweepWithState(ctx, worker.kind, worker.prefix, func(ctx context.Context, entry storage.Entry, p *sourceSweepCheckpoint) error {
				return s.cleanupWorkReceipt(ctx, entry, p, worker.kind == "receipt_cleanup_preview")
			}, nil)
		}()
	}
	for _, worker := range []func(context.Context){s.runStorageOrphanCleanup, s.runDownloadQuotaCleanup} {
		group.Add(1)
		go func() { defer group.Done(); worker(ctx) }()
	}
	group.Wait()
}

func (s *Service) acquireSlot(ctx context.Context, slot chan struct{}) (func(), bool) {
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, true
	case <-ctx.Done():
		return nil, false
	default:
		return nil, false
	}
}

func (s *Service) setRepairPending(pending bool) {
	s.solidificationMu.Lock()
	s.repairPending = pending
	s.solidificationMu.Unlock()
	if s.metrics != nil {
		s.metrics.setReferrerRepairPending(pending)
	}
	if s.tasks != nil {
		s.tasks.WakeDispatch()
	}
}
