package app

import (
	"context"
	"registry-hub/internal/tasks"
	"testing"
	"time"
)

func TestCachedBlobReconciliationPreservesTerminalRounds(t *testing.T) {
	for _, state := range []tasks.BlobIntentState{tasks.IntentCancelled, tasks.IntentFailed, tasks.IntentAnchored} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			f := newStagedAcceptanceFixture(t, 1<<20)
			blob := f.cacheObject([]byte("cached terminal round"), "application/octet-stream")
			identity := tasks.BlobIdentity{Authority: "ghcr.io", Repository: "team/image", Digest: blob.Digest}
			if err := f.service.processCachedBlobIntent(ctx, identity); err != nil {
				t.Fatal(err)
			}
			intent, err := f.service.records.GetBlobIntentByIdentity(ctx, identity)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.records.SetBlobIntentState(ctx, intent.ID, state, "", ""); err != nil {
				t.Fatal(err)
			}
			if state == tasks.IntentCancelled || state == tasks.IntentFailed {
				oldTask, err := f.service.tasks.Get(ctx, intent.ID)
				if err != nil {
					t.Fatal(err)
				}
				owner, err := f.service.tasks.Leases().Acquire(ctx, "tasks/"+oldTask.ID, "terminal-round-fixture", time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				oldTask.Status = tasks.Failed
				if state == tasks.IntentCancelled {
					oldTask.Status, oldTask.CancelRequested, oldTask.CancelPhase = tasks.Cancelled, true, "quiesced"
				}
				oldTask.TerminalCleanupComplete = true
				if err := f.service.tasks.SaveProgress(ctx, &oldTask, owner); err != nil {
					t.Fatal(err)
				}
				if err := f.service.tasks.Leases().Release(ctx, owner); err != nil {
					t.Fatal(err)
				}
			}
			for range 3 {
				if err := f.service.processCachedBlobIntent(ctx, identity); err != nil {
					t.Fatal(err)
				}
			}
			current, err := f.service.records.GetBlobIntentByIdentity(ctx, identity)
			if err != nil || current.ID != intent.ID || current.Round != intent.Round || current.State != state {
				t.Fatalf("background replay replaced terminal round: %+v %v", current, err)
			}
			next, err := f.service.ensureBlobIntentForIdentity(ctx, identity)
			if err != nil || next.ID == intent.ID || next.Round != intent.Round+1 {
				t.Fatalf("explicit client demand did not start a new round: %+v %v", next, err)
			}
		})
	}
}

func TestCachedBlobReconciliationReusesVerifiedBytesAcrossMediaTypes(t *testing.T) {
	ctx := context.Background()
	f := newStagedAcceptanceFixture(t, 1<<20)
	data := []byte("{}")
	blob := f.cacheObject(data, "application/octet-stream")
	identity := tasks.BlobIdentity{Authority: "ghcr.io", Repository: "team/image", Digest: blob.Digest}
	if err := f.service.processCachedBlobIntent(ctx, identity); err != nil {
		t.Fatal(err)
	}
	intent, err := f.service.records.GetBlobIntentByIdentity(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.records.SetBlobIntentState(ctx, intent.ID, tasks.IntentVerified, "", ""); err != nil {
		t.Fatal(err)
	}
	f.cacheObject(data, "application/vnd.oci.image.config.v1+json")
	config, err := f.service.cache.Get(ctx, identity.Authority, identity.Repository, identity.Digest, "application/vnd.oci.image.config.v1+json")
	if err != nil {
		t.Fatal(err)
	}
	defer config.Body.Close()
	if err := f.service.activateBlobIntent(ctx, intent, config); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := f.service.processCachedBlobIntent(ctx, identity); err != nil {
			t.Fatal(err)
		}
	}
	current, err := f.service.records.GetBlobIntentByIdentity(ctx, identity)
	if err != nil || current.ID != intent.ID || current.Round != intent.Round || current.State != tasks.IntentVerified {
		t.Fatalf("alternate media type replaced verified round: id=%s round=%d state=%s err=%v", current.ID, current.Round, current.State, err)
	}
}
