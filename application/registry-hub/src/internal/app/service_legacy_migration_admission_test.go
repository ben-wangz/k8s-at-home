package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func TestLegacyMigrationRetainsBoundedReservationAndRequiresNewScopeAdmission(t *testing.T) {
	ctx := context.Background()
	f := newStagedAcceptanceFixture(t, 128)
	blob := f.cacheObject([]byte("legacy configuration"), "application/octet-stream")
	newLayer := f.cacheObject([]byte("layer outside the grandfathered graph"), "application/octet-stream")
	descriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType,
		Config: &blob, Layers: []artifact.Descriptor{newLayer}})
	root := f.root("legacy-admission", descriptor)
	root.ProtocolVersion = 1
	root.Graph[descriptor.Digest] = tasks.GraphEntry{Digest: descriptor.Digest, MediaType: descriptor.MediaType, Size: descriptor.Size}
	root.Graph[blob.Digest] = tasks.GraphEntry{Digest: blob.Digest, MediaType: blob.MediaType, Size: blob.Size}
	root.Checkpoints[blob.Digest] = "uploaded"
	saveLegacyAcceptanceTask(t, f, &root)
	wantLegacy := descriptor.Size + blob.Size
	if err := f.service.recoverSolidificationProtection(ctx); err != nil {
		t.Fatalf("initial legacy migration: %v", err)
	}
	for round := 0; round < 2; round++ {
		gate, err := newWriteGate(ctx, f.service.store)
		if err != nil {
			t.Fatal(err)
		}
		f.service.gate, f.processor.gate = gate, gate
		if err := f.service.recoverSolidificationProtection(ctx); err != nil {
			t.Fatalf("migration re-entry %d: %v", round, err)
		}
		state := gate.State()
		if state.LegacyGrandfatheredBytes != wantLegacy || state.ReservedDistributionBytes != 0 || state.ProtectionSessionCount != 1 || state.RecoveryBarrier {
			t.Fatalf("legacy migration ledger = %#v", state)
		}
		if err := gate.BeginDrain(ctx); !errors.Is(err, ErrGCProtected) {
			t.Fatalf("GC after legacy migration = %v", err)
		}
	}
	var failure *tasks.Failure
	if err := f.step(root.ID); !errors.As(err, &failure) || !failure.Permanent || failure.Class != "capacity_limit" {
		t.Fatalf("new digest scope bypassed current quota: %v", err)
	}
	current, err := f.service.tasks.Get(ctx, root.ID)
	if err != nil || current.LegacyProtectionBytes != wantLegacy || f.service.gate.State().LegacyGrandfatheredBytes != wantLegacy {
		t.Fatalf("grandfathered reservation expanded: task=%#v error=%v", current, err)
	}
	if f.requests != 0 || len(f.service.solidificationSlots) != 0 {
		t.Fatal("over-limit migrated scope contacted Distribution or retained a worker")
	}
}

func TestInterruptedLegacyOwnerMigrationFailsClosedAndResumes(t *testing.T) {
	ctx := context.Background()
	f := newStagedAcceptanceFixture(t, 1<<20)
	descriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Layers: []artifact.Descriptor{}})
	root := f.root("legacy-owner-failure", descriptor)
	root.ProtocolVersion = 1
	root.Graph[descriptor.Digest] = tasks.GraphEntry{Digest: descriptor.Digest, MediaType: descriptor.MediaType, Size: descriptor.Size}
	root.Checkpoints[descriptor.Digest] = "uploaded"
	saveLegacyAcceptanceTask(t, f, &root)
	faults := &legacyOwnerFailureStore{Store: f.service.store}
	f.service.gate.store = faults
	if err := f.service.recoverSolidificationProtection(ctx); !errors.Is(err, storage.ErrUnavailable) || !faults.triggered {
		t.Fatalf("interrupted migration = %v, fault=%t", err, faults.triggered)
	}
	if !f.service.gate.State().RecoveryBarrier {
		t.Fatal("interrupted owner migration released startup barrier")
	}
	if _, err := f.service.gate.Enter(ctx); !errors.Is(err, ErrRecoveryBarrier) {
		t.Fatalf("write during interrupted migration = %v", err)
	}
	gate, err := newWriteGate(ctx, f.service.store)
	if err != nil {
		t.Fatal(err)
	}
	f.service.gate, f.processor.gate = gate, gate
	if err := f.service.recoverSolidificationProtection(ctx); err != nil {
		t.Fatalf("restart after interrupted migration: %v", err)
	}
	owners, err := gate.ProtectionSessionOwners(ctx, legacyGrandfatheredSessionID)
	if err != nil || len(owners) != 1 || owners[0] != root.ID || gate.State().LegacyGrandfatheredBytes != descriptor.Size || gate.State().RecoveryBarrier {
		t.Fatalf("recovered owner ledger = %v error=%v gate=%#v", owners, err, gate.State())
	}
}

func TestLegacyPublicationAndRepairMigrationPreserveFencesWithoutLocalProof(t *testing.T) {
	for _, stage := range []string{"verified", "publishing", "published-repair"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			f := newStagedAcceptanceFixture(t, 1<<20)
			descriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Layers: []artifact.Descriptor{}})
			root := f.root("legacy-"+stage, descriptor)
			root.ProtocolVersion = 1
			root.Graph[descriptor.Digest] = tasks.GraphEntry{Digest: descriptor.Digest, MediaType: descriptor.MediaType, Size: descriptor.Size}
			root.Checkpoints[descriptor.Digest] = "verified"
			root.RootPublishing = stage != "verified"
			root.RootPublished = stage == "published-repair"
			if root.RootPublished {
				root.Status = tasks.RepairPending
			}
			saveLegacyAcceptanceTask(t, f, &root)
			for range 2 {
				if err := f.service.recoverSolidificationProtection(ctx); err != nil {
					t.Fatal(err)
				}
				current, err := f.service.tasks.Get(ctx, root.ID)
				if err != nil || current.RootPublishing != root.RootPublishing || current.RootPublished != root.RootPublished || current.Status != root.Status {
					t.Fatalf("legacy publication fence changed: %#v error=%v", current, err)
				}
				if _, err := f.service.records.FindLocalObservation(ctx, root.Authority, root.Repository, tasks.GraphManifest, descriptor.Digest, descriptor.Size, root.Generation, f.service.gate.State().Epoch); !errors.Is(err, storage.ErrNotFound) {
					t.Fatalf("legacy verified checkpoint became local proof: %v", err)
				}
				if f.requests != 0 || f.service.gate.State().RecoveryBarrier {
					t.Fatal("migration ran old eager I/O or failed to reconcile")
				}
			}
		})
	}
}

func saveLegacyAcceptanceTask(t *testing.T, f *stagedAcceptanceFixture, task *tasks.Task) {
	t.Helper()
	ctx := context.Background()
	currentLease, err := f.service.tasks.Leases().Acquire(ctx, "tasks/"+task.ID, "legacy-acceptance", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer f.service.tasks.Leases().Release(ctx, currentLease)
	if err := f.service.tasks.SaveProgress(ctx, task, currentLease); err != nil {
		t.Fatal(err)
	}
}

type legacyOwnerFailureStore struct {
	storage.Store
	triggered bool
}

func (s *legacyOwnerFailureStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if !s.triggered && strings.HasPrefix(key, "system/solidification/owners/") {
		s.triggered = true
		return storage.Entry{}, storage.ErrUnavailable
	}
	return s.Store.Put(ctx, key, body, size, options)
}
