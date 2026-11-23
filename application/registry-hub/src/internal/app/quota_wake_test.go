package app

import (
	"bytes"
	"context"
	"encoding/json"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
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
	if err := f.service.enqueueQuotaTransition(ctx, task); err != nil {
		t.Fatal(err)
	}
	entries, _, err := f.service.store.List(ctx, workqueue.Prefix+"jobs/quota_wake/", "", 100)
	if err != nil || len(entries) != 0 {
		t.Fatalf("coalesced dispatch created per-revision queue receipts: %d %v", len(entries), err)
	}
	key := quotaWakeDispatchKey(task.ID)
	dispatch, _, err := inventory.Read[quotaWakeDispatch](ctx, f.service.store, key)
	if err != nil || dispatch.LatestPendingRevision != task.QuotaWakeRevision || dispatch.LatestConfirmedRevision != 0 {
		t.Fatalf("dispatch was not durably pending: %+v %v", dispatch, err)
	}
	if err := f.service.reconcileQuotaWakeDispatch(ctx, key); err != nil {
		t.Fatalf("process coalesced dispatch: %v", err)
	}
	dispatch, _, err = inventory.Read[quotaWakeDispatch](ctx, f.service.store, key)
	if err != nil || dispatch.LatestConfirmedRevision != task.QuotaWakeRevision {
		t.Fatalf("dispatch confirmation was not persisted: %+v %v", dispatch, err)
	}
	current, _ := f.service.tasks.Get(ctx, task.ID)
	if current.Status != tasks.Pending {
		t.Fatalf("target not woken: %+v", current)
	}
	if err = f.service.enqueueQuotaTransition(ctx, task); err != nil {
		t.Fatal(err)
	}
	dispatch, _, err = inventory.Read[quotaWakeDispatch](ctx, f.service.store, key)
	if err != nil || dispatch.LatestPendingRevision != task.QuotaWakeRevision || dispatch.LatestConfirmedRevision != task.QuotaWakeRevision {
		t.Fatalf("reconciliation regressed confirmed dispatch: %+v %v", dispatch, err)
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
	dispatch, _, err = inventory.Read[quotaWakeDispatch](ctx, f.service.store, key)
	if err != nil || dispatch.LatestPendingRevision != next.QuotaWakeRevision || dispatch.LatestConfirmedRevision != task.QuotaWakeRevision {
		t.Fatalf("new task revision was not coalesced: %+v %v", dispatch, err)
	}
	if err := f.service.reconcileQuotaWakeDispatch(ctx, key); err != nil {
		t.Fatalf("process latest coalesced revision: %v", err)
	}
	dispatch, _, err = inventory.Read[quotaWakeDispatch](ctx, f.service.store, key)
	if err != nil || dispatch.LatestConfirmedRevision != next.QuotaWakeRevision {
		t.Fatalf("latest revision was not confirmed: %+v %v", dispatch, err)
	}
	stalePayload, _ := json.Marshal(quotaWakeWork{TaskID: task.ID, Generation: task.Generation, RoundID: task.RoundID, Revision: task.QuotaWakeRevision, SessionID: task.ProtectionSessionID})
	stale := workqueue.Job{Payload: stalePayload}
	if _, done, staleErr := f.service.quotaWakeStep(ctx, &stale); staleErr != nil || !done || !stale.Superseded {
		t.Fatalf("stale event not superseded: %+v %v", stale, staleErr)
	}
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
