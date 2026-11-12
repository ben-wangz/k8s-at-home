package app

import (
	"bytes"
	"context"
	"errors"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"testing"
	"time"
)

func TestDiscoverySnapshotRecoversFromCorruptAccelerationRecords(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	blob := artifact.Descriptor{Digest: artifact.CalculateSHA256([]byte("cold")), Size: 4, MediaType: "application/octet-stream"}
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Layers: []artifact.Descriptor{blob}})
	root := f.root("corrupt-discovery", manifest)
	_ = f.step(root.ID)
	for _, key := range []string{discoverySnapshotKey(root.ID), graphExecutionKey(root.ID), graphSignalPrefix(root.ID) + "damaged.json"} {
		if _, err := f.service.store.Put(context.Background(), key, bytes.NewBufferString("invalid JSON"), 12, storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	var yielded *tasks.Yield
	if err := f.step(root.ID); !errors.As(err, &yielded) || yielded.Status != tasks.WaitingDeps {
		t.Fatalf("corrupt optional records blocked full discovery: %v", err)
	}
	snapshot, _, err := inventory.Read[discoverySnapshot](context.Background(), f.service.store, discoverySnapshotKey(root.ID))
	if err != nil || snapshot.Revision == "" {
		t.Fatalf("discovery record was not repaired: %+v, %v", snapshot, err)
	}
}

func TestGraphIncrementalStateCannotCrossGCEpoch(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	epoch := f.service.gate.State().Epoch
	ctx := context.WithValue(context.Background(), graphEpochKey{}, epoch)
	if err := f.processor.checkGraphEpoch(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.service.gate.BeginDrain(context.Background()); err != nil {
		t.Fatal(err)
	}
	var yielded *tasks.Yield
	if err := f.processor.checkGraphEpoch(ctx); !errors.As(err, &yielded) || yielded.Reason != "gc_epoch_changed" {
		t.Fatalf("stale readiness reused across GC: %v", err)
	}
}

func TestCacheVerificationUsesSharedPayloadCapacityAndWorkerDiagnostics(t *testing.T) {
	for _, kind := range []string{"cache_verification", "download_cleanup"} {
		t.Run(kind, func(t *testing.T) {
			f := newStagedAcceptanceFixture(t, 1<<20)
			ctx, release, ok := f.service.beginQueueUnit(context.Background(), kind)
			if !ok || len(f.service.solidificationSlots) != 1 {
				t.Fatal("verification did not reserve shared payload capacity")
			}
			defer release()
			if queueClaimDuration(ctx, kind) <= 4*time.Minute {
				t.Fatal("verification claim does not cover its payload budget")
			}
			state := f.service.workerStates[kind]
			if state.State != "execution" || state.RunID == "" || state.ExecutionBudgetSeconds < 300 {
				t.Fatalf("verification execution is missing from workers: %+v", state)
			}
			view := workerView{workerStatus: state}
			f.service.configureWorkerBudgets(&view)
			if view.AdmissionPool != "solidification" || view.AdmissionCapacity != 1 || view.ExecutionBudgetSeconds < 300 {
				t.Fatalf("worker API reports the wrong verification capacity or budget: %+v", view)
			}
		})
	}
}
