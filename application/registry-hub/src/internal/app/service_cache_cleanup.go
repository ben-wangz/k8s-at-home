package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"strings"
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
		{"receipt_cleanup", "background/v1/receipts/", func(ctx context.Context, entry storage.Entry) error {
			if strings.Contains(entry.Key, "/execution/") || entry.ModTime == 0 || time.Since(time.Unix(0, entry.ModTime)) < 30*24*time.Hour {
				return nil
			}
			return s.deleteDiagnosticObservation(ctx, entry)
		}},
	}
	var group sync.WaitGroup
	for _, worker := range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			if worker.kind != "receipt_cleanup" {
				s.runSourceSweep(ctx, worker.kind, worker.prefix, worker.step)
				return
			}
			s.runSourceSweepWithState(ctx, worker.kind, worker.prefix, func(ctx context.Context, entry storage.Entry, p *sourceSweepCheckpoint) error {
				if strings.Contains(entry.Key, "/execution/") || entry.ModTime == 0 || time.Since(time.Unix(0, entry.ModTime)) < 30*24*time.Hour {
					incrementSweepCount(p, "skipped", 1)
					return nil
				}
				kind, id, ok := receiptWorkIdentity(entry.Key)
				if !ok {
					incrementSweepCount(p, "skipped", 1)
					return nil
				}
				job, _, err := inventory.Read[workqueue.Job](ctx, s.store, workqueue.Key(kind, id))
				if err == nil {
					if job.ID != id || job.Kind != kind {
						return storage.ErrCorruptObject
					}
					incrementSweepCount(p, "skipped", 1)
					return nil
				}
				if !errors.Is(err, storage.ErrNotFound) {
					return err
				}
				if err := s.deleteDiagnosticObservation(ctx, entry); err != nil {
					return err
				}
				incrementSweepCount(p, "deleted", 1)
				return nil
			}, nil)
		}()
	}
	for _, worker := range []func(context.Context){s.runStorageOrphanCleanup, s.runDownloadQuotaCleanup} {
		group.Add(1)
		go func() { defer group.Done(); worker(ctx) }()
	}
	group.Wait()
}

func receiptWorkIdentity(key string) (string, string, bool) {
	relative := strings.TrimPrefix(key, "background/v1/receipts/")
	parts := strings.SplitN(relative, "/", 2)
	if len(parts) != 2 || parts[0] == "" || !strings.HasSuffix(parts[1], ".json") {
		return "", "", false
	}
	return parts[0], strings.TrimSuffix(parts[1], ".json"), true
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
