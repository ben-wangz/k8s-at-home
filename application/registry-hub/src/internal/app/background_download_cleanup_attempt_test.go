package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/download"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"strings"
	"testing"
	"time"
)

func TestDownloadCleanupVerifiesOnceAcrossBatchesAndReleasesQuota(t *testing.T) {
	f := newDownloadAttemptFixture(t, 70)
	f.claim(t)
	progress, done, err := f.service.cleanupDownloadStep(context.Background(), &f.job)
	if err != nil || !done || f.store.checkpoints < 5 {
		t.Fatalf("cleanup done=%t checkpoints=%d error=%v", done, f.store.checkpoints, err)
	}
	if got := f.store.readBytes.Load(); got != f.size {
		t.Fatalf("multi-batch cleanup read %d payload bytes, want %d", got, f.size)
	}
	if progress.(download.CleanupProgress).Phase != 2 {
		t.Fatalf("progress=%+v", progress)
	}
	if reserved, count, err := f.service.downloadManager().Usage(context.Background()); err != nil || reserved != 0 || count != 0 {
		t.Fatalf("quota=%d count=%d error=%v", reserved, count, err)
	}
	f.assertReleased(t)
}

func TestDownloadCleanupStorageWorkSharesReaderPinCancellation(t *testing.T) {
	f := newDownloadAttemptFixture(t, 30)
	f.claim(t)
	checked := 0
	f.store.beforeDelete = func(ctx context.Context) error {
		if f.store.verifiedCtx == nil || ctx.Done() != f.store.verifiedCtx.Done() {
			return storage.ErrLeaseLost
		}
		checked++
		return nil
	}
	_, done, err := f.service.cleanupDownloadStep(context.Background(), &f.job)
	if err != nil || !done || checked < 30 {
		t.Fatalf("protected deletion contexts=%d done=%t error=%v", checked, done, err)
	}
	f.assertReleased(t)
}

func TestDownloadCleanupPersistsProgressAndReverifiesAfterRestart(t *testing.T) {
	f := newDownloadAttemptFixture(t, downloadCleanupAttemptBatches*16+40)
	f.claim(t)
	ctx := context.Background()
	progress, done, err := f.service.cleanupDownloadStep(ctx, &f.job)
	if err != nil || done || f.store.checkpoints != downloadCleanupAttemptBatches {
		t.Fatalf("bounded attempt done=%t checkpoints=%d error=%v", done, f.store.checkpoints, err)
	}
	f.assertReleased(t)
	q := f.service.backgroundQueue()
	persisted, err := q.Read(ctx, f.job.Kind, f.job.ID)
	if err != nil || string(persisted.Progress) != string(f.job.Progress) || len(persisted.Progress) == 0 {
		t.Fatalf("missing durable progress: %+v, %v", persisted, err)
	}
	if err := q.Finish(ctx, &f.job, progress, false, nil); err != nil {
		t.Fatal(err)
	}
	// Recreate attempt-local application state and load only the persisted progress.
	f.service.downloader = download.New(f.store, 1<<20)
	f.job, err = q.Read(ctx, f.job.Kind, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.claim(t)
	_, done, err = f.service.cleanupDownloadStep(ctx, &f.job)
	if err != nil || !done || f.store.readBytes.Load() != 2*f.size {
		t.Fatalf("restart done=%t bytes=%d error=%v", done, f.store.readBytes.Load(), err)
	}
	f.assertReleased(t)
}

func TestDownloadCleanupStopsAfterFailedCheckpointAndReverifiesRetry(t *testing.T) {
	f := newDownloadAttemptFixture(t, 70)
	f.claim(t)
	f.store.beforeSave = func() error { return storage.ErrUnavailable }
	progress, done, err := f.service.cleanupDownloadStep(context.Background(), &f.job)
	if !errors.Is(err, storage.ErrUnavailable) || done || f.store.checkpoints != 0 {
		t.Fatalf("failed checkpoint done=%t checkpoints=%d error=%v", done, f.store.checkpoints, err)
	}
	f.assertReleased(t)
	f.store.beforeSave = nil
	q := f.service.backgroundQueue()
	if err := q.Finish(context.Background(), &f.job, progress, false, nil); err != nil {
		t.Fatal(err)
	}
	f.claim(t)
	_, done, err = f.service.cleanupDownloadStep(context.Background(), &f.job)
	if err != nil || !done || f.store.readBytes.Load() != 2*f.size {
		t.Fatalf("retry done=%t bytes=%d error=%v", done, f.store.readBytes.Load(), err)
	}
	f.assertReleased(t)
}

func TestDownloadCleanupRejectsStaleFenceBetweenBatches(t *testing.T) {
	for _, mode := range []string{"queue", "object", "generation", "recovery", "session", "cache", "pin"} {
		t.Run(mode, func(t *testing.T) {
			f := newDownloadAttemptFixture(t, 70)
			f.claim(t)
			f.store.afterSave = func() {
				f.store.afterSave = nil
				invalidateDownloadAttempt(t, &f, mode)
			}
			_, done, err := f.service.cleanupDownloadStep(context.Background(), &f.job)
			if err == nil || done || f.store.checkpoints != 1 {
				t.Fatalf("stale %s done=%t checkpoints=%d error=%v", mode, done, f.store.checkpoints, err)
			}
			f.assertReleased(t)
		})
	}
}

func invalidateDownloadAttempt(t *testing.T, f *downloadAttemptFixture, mode string) {
	t.Helper()
	ctx := context.Background()
	var err error
	switch mode {
	case "queue":
		err = inventory.Update(ctx, f.store.Store, workqueue.Key(f.job.Kind, f.job.ID), func(job *workqueue.Job) error { job.Fence++; return nil })
	case "object":
		entries, _, listErr := storage.Scan(ctx, f.store, "leases/", "", 100)
		err = listErr
		for _, entry := range entries {
			if err == nil {
				err = f.store.Delete(ctx, entry.Key)
			}
		}
	case "generation":
		_, err = f.service.advanceGeneration(ctx, f.source.Authority, f.source.Repository, f.source.Digest, true)
	case "recovery":
		f.service.gate = &writeGate{state: gateRecord{RecoveryBarrier: true}}
	case "session":
		err = inventory.Update(ctx, f.store, f.prefix+"state.json", func(state *map[string]any) error { (*state)["revision"] = 100; return nil })
	case "cache", "pin":
		prefix := "cache/objects/"
		if mode == "pin" {
			prefix = "cache/readers/"
		}
		entries, _, listErr := storage.Scan(ctx, f.store, prefix, "", 100)
		err = listErr
		for _, entry := range entries {
			if err == nil && (mode == "pin" || strings.HasSuffix(entry.Key, "/data")) {
				err = f.store.Delete(ctx, entry.Key)
			}
		}
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestDownloadCleanupCancellationRetainsQuotaAndDurableProgress(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			f := newDownloadAttemptFixture(t, 70)
			f.claim(t)
			ctx, cancel := context.WithCancel(context.Background())
			want := context.Canceled
			if mode == "timeout" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
				want = context.DeadlineExceeded
			}
			defer cancel()
			ctx = context.WithValue(ctx, backgroundParentKey{}, context.Background())
			f.store.afterSave = func() {
				if mode == "cancel" {
					cancel()
				}
				<-ctx.Done()
			}
			_, done, err := f.service.cleanupDownloadStep(ctx, &f.job)
			if !errors.Is(err, want) || done || f.store.checkpoints != 1 {
				t.Fatalf("interrupted done=%t checkpoints=%d error=%v", done, f.store.checkpoints, err)
			}
			persisted, readErr := f.service.backgroundQueue().Read(context.Background(), f.job.Kind, f.job.ID)
			var progress download.CleanupProgress
			if readErr != nil || json.Unmarshal(persisted.Progress, &progress) != nil || progress.Token == "" {
				t.Fatalf("partial progress=%+v error=%v", progress, readErr)
			}
			if reserved, count, err := f.service.downloadManager().Usage(context.Background()); err != nil || reserved == 0 || count != 1 {
				t.Fatalf("quota lost: %d/%d error=%v", reserved, count, err)
			}
			f.assertReleased(t)
		})
	}
}
