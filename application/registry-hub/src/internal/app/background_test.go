package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

func drainBackgroundWork(t *testing.T, s *Service, kinds ...string) {
	t.Helper()
	for n := 0; n < 2000; n++ {
		pending, err := backgroundTestPass(s, kinds...)
		if err != nil {
			t.Fatal(err)
		}
		if !pending {
			return
		}
	}
	t.Fatal("background work did not converge")
}

func backgroundUntilError(s *Service, kinds ...string) error {
	for n := 0; n < 2000; n++ {
		pending, err := backgroundTestPass(s, kinds...)
		if err != nil || !pending {
			return err
		}
	}
	return errors.New("background work did not converge")
}

func backgroundTestPass(s *Service, kinds ...string) (bool, error) {
	ctx := context.Background()
	q := workqueue.Queue{Store: s.store}
	steps := map[string]backgroundStep{"inventory": s.projectManifestStep, "execution": s.projectExecutionStep, "download_cleanup": s.cleanupDownloadStep, "notify": s.notificationStep, "wake_root": s.wakeRootStep}
	pending := false
	for _, kind := range kinds {
		cursor := inventory.Cursor{}
		for !cursor.Done {
			entry, ok, err := cursor.Peek(ctx, s.store, workqueue.Prefix+"jobs/"+kind+"/", 32)
			if err != nil {
				return pending, err
			}
			if !ok {
				continue
			}
			id := strings.TrimSuffix(entry.Key[strings.LastIndex(entry.Key, "/")+1:], ".json")
			job, err := q.Read(ctx, kind, id)
			if err != nil {
				return pending, err
			}
			if job.State == "done" {
				err = q.Archive(ctx, job)
			} else {
				pending = true
				err = backgroundTestJob(ctx, q, job, steps[kind])
			}
			if err != nil {
				return pending, err
			}
			cursor.Advance()
		}
	}
	return pending, nil
}

func backgroundTestJob(ctx context.Context, q workqueue.Queue, job workqueue.Job, step backgroundStep) error {
	if job.State == "retry_wait" || job.State == "failed" {
		if err := q.Retry(ctx, job.Kind, job.ID); err != nil {
			return err
		}
		var err error
		job, err = q.Read(ctx, job.Kind, job.ID)
		if err != nil {
			return err
		}
	}
	claimed, err := q.Claim(ctx, job)
	if errors.Is(err, storage.ErrConditionFailed) {
		return nil
	}
	if err != nil {
		return err
	}
	guarded := inventory.WithWriteGuard(ctx, func(ctx context.Context) error { return q.Validate(ctx, claimed) })
	progress, done, cause := step(guarded, &claimed)
	return errors.Join(cause, q.Finish(ctx, &claimed, progress, done, cause))
}
