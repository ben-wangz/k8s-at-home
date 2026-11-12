package app

import (
	"bytes"
	"context"
	"encoding/json"
	"registry-hub/internal/artifact"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"testing"
	"time"
)

func quotaCoveredFixture(t *testing.T) (*stagedAcceptanceFixture, tasks.Task) {
	t.Helper()
	f := newStagedAcceptanceFixture(t, 64)
	f.service.cfg.Solidification.MaxProtectionSessions = 1
	ctx := context.Background()
	body := []byte("cached quota blob")
	digest := artifact.CalculateSHA256(body)
	if err := f.service.cache.Put(ctx, "ghcr.io", "team/image", digest, "application/octet-stream", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	cached, err := f.service.cache.Inspect(ctx, "ghcr.io", "team/image", digest, "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	intent, _, err := f.service.records.EnsureBlobIntent(ctx, tasks.BlobIdentity{Authority: "ghcr.io", Repository: "team/image", Digest: digest}, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	intent, err = f.service.records.SetBlobIntentReady(ctx, intent.ID, cached.MediaType, cached.DataVersion, cached.Size)
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := f.service.tasks.EnqueueBlob(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	if err = saveDemandRootStatus(ctx, f.service.tasks, task.ID, tasks.WaitingQuota, "distribution_quota"); err != nil {
		t.Fatal(err)
	}
	session := mustGateTestID(t)
	root := newDemandRootTask(t, ctx, f.service.tasks, "covered-quota")
	if err = f.service.gate.AdmitProtection(ctx, session, "manifest-scope", root.ID, 64, 1, 64); err != nil {
		t.Fatal(err)
	}
	linkRootBlobDemand(t, ctx, f.service.records, intent.ID, root.ID, mustGateTestID(t), "manifest-scope", session)
	if err = f.service.gate.AddProtectionOwner(ctx, session, task.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.service.records.SetBlobIntentProtectionSession(ctx, intent.ID, session); err != nil {
		t.Fatal(err)
	}
	if err = f.service.tasks.ReconcileProtectionState(ctx, task.ID, "manifest-scope", []string{session}, 64, 64); err != nil {
		t.Fatal(err)
	}
	task, err = f.service.tasks.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, task
}

func TestQuotaCoveredBlobWakesWithNoAdditionalCapacity(t *testing.T) {
	f, task := quotaCoveredFixture(t)
	ctx := context.Background()
	older := newDemandRootTask(t, ctx, f.service.tasks, "older-uncovered")
	if err := saveDemandRootStatus(ctx, f.service.tasks, older.ID, tasks.WaitingQuota, "distribution_quota"); err != nil {
		t.Fatal(err)
	}
	before := f.service.gate.State()
	if err := f.service.wakeFittingQuotaWaiter(ctx, "distribution_quota", task, before); err != nil {
		t.Fatal(err)
	}
	current, err := f.service.tasks.Get(ctx, task.ID)
	if err != nil || current.Status != tasks.Pending || current.ExecutableAt.IsZero() {
		t.Fatalf("covered blob not runnable: %+v %v", current, err)
	}
	after := f.service.gate.State()
	if after.ReservedDistributionBytes != 64 || after.NewProtectionSessionCount != 1 || after.ProtectedCacheBytes != before.ProtectedCacheBytes {
		t.Fatalf("wake changed reservations: %+v", after)
	}
	uncovered, _ := f.service.tasks.Get(ctx, older.ID)
	if err = f.service.wakeFittingQuotaWaiter(ctx, "distribution_quota", uncovered, after); err != nil {
		t.Fatal(err)
	}
	uncovered, _ = f.service.tasks.Get(ctx, older.ID)
	if uncovered.Status != tasks.WaitingQuota {
		t.Fatal("uncovered task bypassed capacity")
	}
}

func TestQuotaTransitionReplayWakesTargetInFirstAdmission(t *testing.T) {
	f, task := quotaCoveredFixture(t)
	ctx := context.Background()
	// A new service can reconstruct delivery solely from the committed task.
	if err := f.service.enqueueQuotaTransition(ctx, task); err != nil {
		t.Fatal(err)
	}
	entries, _, err := f.service.store.List(ctx, workqueue.Prefix+"jobs/quota_wake/", "", 100)
	if err != nil || len(entries) != 1 {
		t.Fatalf("missing durable wake: %v %v", entries, err)
	}
	q := workqueue.Queue{Store: f.service.store}
	job, _, err := readQuotaJob(ctx, f.service, entries[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	job, err = q.Claim(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	p, done, err := f.service.quotaWakeStep(ctx, &job)
	if err != nil || !done {
		t.Fatalf("wake needs another admission: %v %v", done, err)
	}
	if err = q.Finish(ctx, &job, p, done, nil); err != nil {
		t.Fatal(err)
	}
	current, _ := f.service.tasks.Get(ctx, task.ID)
	if current.Status != tasks.Pending {
		t.Fatalf("target not woken: %+v", current)
	}
	if err = f.service.enqueueQuotaTransition(ctx, task); err != nil {
		t.Fatal(err)
	}
	if replay, _ := q.Read(ctx, job.Kind, job.ID); replay.State != "done" {
		t.Fatal("duplicate delivery revived receipt")
	}
	if err = saveDemandRootStatus(ctx, f.service.tasks, task.ID, tasks.WaitingQuota, "distribution_quota"); err != nil {
		t.Fatal(err)
	}
	next, _ := f.service.tasks.Get(ctx, task.ID)
	if next.QuotaWakeRevision == task.QuotaWakeRevision {
		t.Fatal("repeated transition reused revision")
	}
	if err = f.service.enqueueQuotaTransition(ctx, next); err != nil {
		t.Fatal(err)
	}
	stale := workqueue.Job{Payload: job.Payload}
	if _, done, err = f.service.quotaWakeStep(ctx, &stale); err != nil || !done || !stale.Superseded {
		t.Fatalf("stale event not superseded: %+v %v", stale, err)
	}
}

func readQuotaJob(ctx context.Context, s *Service, key string) (workqueue.Job, string, error) {
	object, err := s.store.Get(ctx, key)
	if err != nil {
		return workqueue.Job{}, "", err
	}
	defer object.Body.Close()
	var job workqueue.Job
	err = json.NewDecoder(object.Body).Decode(&job)
	job.Version = object.Version
	return job, object.Version, err
}

func TestQuotaCoveredWakeRejectsCancellationAndOtherWaits(t *testing.T) {
	for _, reason := range []string{"protected_cache_capacity", "distribution_quota"} {
		t.Run(reason, func(t *testing.T) {
			f, task := quotaCoveredFixture(t)
			ctx := context.Background()
			if err := saveDemandRootStatus(ctx, f.service.tasks, task.ID, tasks.WaitingQuota, reason); err != nil {
				t.Fatal(err)
			}
			task, _ = f.service.tasks.Get(ctx, task.ID)
			if reason == "distribution_quota" {
				if _, err := f.service.tasks.Cancel(tasks.WithDeferredTerminalCleanup(ctx), task.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.service.wakeCoveredBlob(ctx, task); err != nil {
				t.Fatal(err)
			}
			current, _ := f.service.tasks.Get(ctx, task.ID)
			if current.Status == tasks.Pending {
				t.Fatalf("ineligible task revived: %+v", current)
			}
		})
	}
}

func TestQuotaSignalPersistsAndDoesNotAcknowledgeMidRound(t *testing.T) {
	f, task := quotaCoveredFixture(t)
	ctx := context.Background()
	payload, _ := json.Marshal(quotaWakeWork{TaskID: task.ID, Generation: task.Generation, RoundID: task.RoundID, Revision: task.QuotaWakeRevision, SessionID: task.ProtectionSessionID})
	job := workqueue.Job{Payload: payload}
	if _, done, err := f.service.quotaWakeStep(ctx, &job); err != nil || !done {
		t.Fatalf("signal: %v", err)
	}
	p := sourceSweepCheckpoint{NextRound: time.Now().Add(time.Minute)}
	p.Cursor.Entities = 16
	if err := f.service.refreshQuotaSignal(ctx, &p); err != nil {
		t.Fatal(err)
	}
	if !p.NextRound.IsZero() || p.QuotaSignalRevision != 0 {
		t.Fatalf("mid-round acknowledged signal: %+v", p)
	}
	p.Cursor.Entities = 0
	if err := f.service.refreshQuotaSignal(ctx, &p); err != nil {
		t.Fatal(err)
	}
	if p.QuotaSignalRevision == 0 {
		t.Fatal("restart lost signal")
	}
}
