package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func TestFreshDemandClearsReleasedCancelledProtectionBeforeStartingNextRound(t *testing.T) {
	ctx := context.Background()
	fixture := newStagedAcceptanceFixture(t, 1<<20)
	identity, old := newCancelledIntentWithProtection(t, ctx, fixture)
	if err := releaseCancelledProtection(t, ctx, fixture, old); err != nil {
		t.Fatal(err)
	}
	tombstone, err := fixture.service.advanceGeneration(ctx, identity.Authority, identity.Repository, identity.Digest, true)
	if err != nil {
		t.Fatal(err)
	}

	next, err := fixture.service.ensureBlobIntentForIdentity(ctx, identity)
	if err != nil {
		t.Fatalf("fresh demand after released protection: %v", err)
	}
	currentGeneration, err := fixture.service.generation(ctx, identity.Authority, identity.Repository, identity.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if currentGeneration.Generation != tombstone.Generation+1 || currentGeneration.Tombstone {
		t.Fatalf("generation after fresh demand = %#v, want tombstone advanced once and cleared", currentGeneration)
	}
	if next.ID == old.ID || next.Round != old.Round+1 || next.Generation != currentGeneration.Generation {
		t.Fatalf("next intent = %#v, want next round at current generation %d", next, currentGeneration.Generation)
	}
	retained, err := fixture.service.records.GetBlobIntent(ctx, old.ID)
	if err != nil || retained.State != tasks.IntentCancelled || retained.ProtectionSessionID != "" {
		t.Fatalf("historical intent after admission = %#v err=%v", retained, err)
	}
	session, err := fixture.service.gate.readProtectionSession(ctx, old.ProtectionSessionID)
	if err != nil || session.State != "released" {
		t.Fatalf("historical protection session = %#v err=%v", session, err)
	}
}

func TestFreshDemandWaitsForActiveOrUnknownCancelledProtection(t *testing.T) {
	for _, mode := range []string{"active", "missing", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			fixture := newStagedAcceptanceFixture(t, 1<<20)
			identity, old := newCancelledIntentWithProtection(t, ctx, fixture)
			switch mode {
			case "active":
				if err := fixture.service.gate.AdmitProtection(ctx, old.ProtectionSessionID, "blob-scope", old.ID, 64, 4, 1<<20); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if _, err := fixture.service.store.Put(ctx, protectionSessionKey(old.ProtectionSessionID), strings.NewReader("not-json"), int64(len("not-json")), storage.PutOptions{IfAbsent: true}); err != nil {
					t.Fatal(err)
				}
			}

			_, err := fixture.service.ensureBlobIntentForIdentity(ctx, identity)
			if !errors.Is(err, tasks.ErrDemandAdmissionWaiting) || !errors.Is(err, storage.ErrConditionFailed) {
				t.Fatalf("fresh demand error = %v, want cleanup wait", err)
			}
			current, readErr := fixture.service.records.GetBlobIntentByIdentity(ctx, identity)
			if readErr != nil || current.ID != old.ID || current.Round != old.Round || current.ProtectionSessionID != old.ProtectionSessionID {
				t.Fatalf("current intent after blocked admission = %#v err=%v", current, readErr)
			}
			generation, readErr := fixture.service.generation(ctx, identity.Authority, identity.Repository, identity.Digest)
			if readErr != nil || generation.Generation != old.Generation || generation.Tombstone {
				t.Fatalf("generation after blocked admission = %#v err=%v", generation, readErr)
			}
		})
	}
}

func TestFreshDemandClearsTransferredCancelledProtection(t *testing.T) {
	ctx := context.Background()
	fixture := newStagedAcceptanceFixture(t, 1<<20)
	identity, old := newCancelledIntentWithProtection(t, ctx, fixture)
	newSessionID := mustGateTestID(t)
	if err := fixture.service.gate.AdmitProtection(ctx, old.ProtectionSessionID, "blob-scope", old.ID, 64, 4, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.gate.ReplaceProtection(ctx, old.ProtectionSessionID, newSessionID, "root-scope", 128, 4, 1<<20); err != nil {
		t.Fatal(err)
	}

	next, err := fixture.service.ensureBlobIntentForIdentity(ctx, identity)
	if err != nil || next.ID == old.ID || next.Round != old.Round+1 {
		t.Fatalf("fresh demand after protection transfer = %#v err=%v", next, err)
	}
	prior, err := fixture.service.gate.readProtectionSession(ctx, old.ProtectionSessionID)
	if err != nil || prior.State != "transferred" || prior.TerminalReason != "scope_merged" {
		t.Fatalf("transferred session history = %#v err=%v", prior, err)
	}
}

func TestConcurrentFreshDemandAfterReleasedProtectionUsesOneRound(t *testing.T) {
	ctx := context.Background()
	fixture := newStagedAcceptanceFixture(t, 1<<20)
	identity, old := newCancelledIntentWithProtection(t, ctx, fixture)
	if err := releaseCancelledProtection(t, ctx, fixture, old); err != nil {
		t.Fatal(err)
	}

	const callers = 6
	start := make(chan struct{})
	results := make(chan tasks.BlobIntent, callers)
	errs := make(chan error, callers)
	var workers sync.WaitGroup
	for range callers {
		service := &Service{
			store:   fixture.service.store,
			records: tasks.NewRecords(fixture.service.store),
			gate:    fixture.service.gate,
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			intent, err := service.ensureBlobIntentForIdentity(ctx, identity)
			if err != nil {
				errs <- err
				return
			}
			results <- intent
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var sharedID string
	for intent := range results {
		if sharedID == "" {
			sharedID = intent.ID
		}
		if intent.ID != sharedID || intent.Round != old.Round+1 {
			t.Fatalf("concurrent fresh intent = %#v, want one shared next round", intent)
		}
	}
	current, err := fixture.service.records.GetBlobIntentByIdentity(ctx, identity)
	if err != nil || current.ID != sharedID {
		t.Fatalf("current intent after concurrent admission = %#v err=%v", current, err)
	}
}

func newCancelledIntentWithProtection(t *testing.T, ctx context.Context, fixture *stagedAcceptanceFixture) (tasks.BlobIdentity, tasks.BlobIntent) {
	t.Helper()
	identity := tasks.BlobIdentity{
		Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("f", 64),
	}
	generation, err := fixture.service.advanceGeneration(ctx, identity.Authority, identity.Repository, identity.Digest, false)
	if err != nil {
		t.Fatal(err)
	}
	intent, _, err := fixture.service.records.EnsureBlobIntent(ctx, identity, generation.Generation, "")
	if err != nil {
		t.Fatal(err)
	}
	intent, err = fixture.service.records.SetBlobIntentState(ctx, intent.ID, tasks.IntentCancelled, "no_demand", "no demand remains")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.records.SetBlobIntentProtectionSession(ctx, intent.ID, mustGateTestID(t)); err != nil {
		t.Fatal(err)
	}
	intent, err = fixture.service.records.GetBlobIntent(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	return identity, intent
}

func releaseCancelledProtection(t *testing.T, ctx context.Context, fixture *stagedAcceptanceFixture, intent tasks.BlobIntent) error {
	t.Helper()
	if err := fixture.service.gate.AdmitProtection(ctx, intent.ProtectionSessionID, "blob-scope", intent.ID, 64, 4, 1<<20); err != nil {
		return err
	}
	if owners, err := fixture.service.gate.RemoveProtectionOwner(ctx, intent.ProtectionSessionID, intent.ID); err != nil || owners != 0 {
		return errors.Join(err, errors.New("protection session retained an owner"))
	}
	if stopping, err := fixture.service.gate.RequestProtectionStop(ctx, intent.ProtectionSessionID, "cancelled"); err != nil || !stopping {
		return errors.Join(err, errors.New("protection session did not enter stop"))
	}
	if err := fixture.service.gate.MarkProtectionQuiescing(ctx, intent.ProtectionSessionID); err != nil {
		return err
	}
	if err := fixture.service.gate.MarkProtectionQuiesced(ctx, intent.ProtectionSessionID); err != nil {
		return err
	}
	return fixture.service.gate.ReleaseProtection(ctx, intent.ProtectionSessionID, "cancelled")
}
