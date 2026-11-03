package app

import (
	"context"
	"errors"
	"fmt"
	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
)

func TestProtectionRecoveryRetriesTerminalDemandDeleteUnknownCommit(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint(committed), func(t *testing.T) {
			f, faults := newRecoveryFixture(t)
			ctx := context.Background()
			intent := newReadyBlobIntent(t, ctx, f.service.tasks, f.service.records)
			root := newDemandRootTask(t, ctx, f.service.tasks, "terminal-delete")
			linkRootBlobDemand(t, ctx, f.service.records, intent.ID, root.ID, strings.Repeat("1", 32), "scope", strings.Repeat("a", 32))
			root, err := f.service.tasks.Cancel(ctx, root.ID)
			if err != nil {
				t.Fatal(err)
			}
			failed := false
			faults.deleteHook = func(key string) error {
				if !failed && strings.HasPrefix(key, "solidification/blobs/root-owners/") {
					failed = true
					if committed {
						entry, err := faults.Store.Head(ctx, key)
						if err != nil {
							return err
						}
						if err := faults.Store.DeleteIfMatch(ctx, key, entry.Version); err != nil {
							return err
						}
					}
					return storage.ErrUnknownCommit
				}
				return nil
			}
			if err := f.processor.TaskTerminal(ctx, &root); !errors.Is(err, storage.ErrUnknownCommit) || !failed {
				t.Fatalf("terminal fault not reproduced: %v", err)
			}
			if !f.service.gate.State().RecoveryBarrier {
				t.Fatal("terminal failure did not install a recovery barrier")
			}
			if err := f.service.recoverSolidificationProtection(ctx); err != nil {
				t.Fatal(err)
			}
			current, err := f.service.tasks.Get(ctx, root.ID)
			if err != nil || !current.TerminalCleanupComplete || current.RecoveryReason != "" {
				t.Fatalf("terminal cleanup did not converge: %+v %v", current, err)
			}
			if demands, _, err := f.service.records.ListRootBlobDemandRecords(ctx, root.ID, "", 1); err != nil || len(demands) != 0 {
				t.Fatalf("terminal demand survived: %v %v", demands, err)
			}
		})
	}
}

func TestProtectionRecoveryResolvesUnknownFinalCommitWithoutRescanning(t *testing.T) {
	f, faults := newRecoveryFixture(t)
	ctx := context.Background()
	failed, rejectReadback := false, false
	faults.putHook = func(key string, data []byte) error {
		if key != "system/gc/state.json" || failed || !strings.Contains(string(data), `"recoveryBarrier"`) && !strings.Contains(string(data), `"recoveryID"`) {
			return nil
		}
		if strings.Contains(string(data), `"recoveryBarrier":true`) {
			return nil
		}
		entry, err := faults.Store.Head(ctx, key)
		if err != nil {
			return err
		}
		if _, err := faults.Store.Put(ctx, key, strings.NewReader(string(data)), int64(len(data)), storage.PutOptions{IfMatch: entry.Version}); err != nil {
			return err
		}
		failed, rejectReadback = true, true
		return storage.ErrUnknownCommit
	}
	faults.getHook = func(key string) error {
		if rejectReadback && key == "system/gc/state.json" {
			rejectReadback = false
			return storage.ErrUnavailable
		}
		return nil
	}
	if err := f.service.recoverSolidificationProtection(ctx); !errors.Is(err, storage.ErrUnknownCommit) {
		t.Fatalf("final commit was not unknown: %v", err)
	}
	before := recoveryCheckpointForTest(t, f.service)
	if before.Phase != "finalize" || !f.service.gate.recovery.blocked.Load() {
		t.Fatalf("completion evidence or freeze was lost: %+v", before)
	}
	if err := f.service.recoverSolidificationProtection(ctx); err != nil {
		t.Fatal(err)
	}
	after := recoveryCheckpointForTest(t, f.service)
	if after.Generation != before.Generation || after.Processed != before.Processed+1 || after.Phase != "complete" || f.service.gate.State().RecoveryBarrier || f.service.gate.State().State != "open" {
		t.Fatalf("confirmed completion restarted recovery: before=%+v after=%+v", before, after)
	}
}

func TestProtectionRecoveryRestartsAfterEveryWorkUnitCheckpointFailure(t *testing.T) {
	f, faults := newRecoveryFixture(t)
	ctx := context.Background()
	descriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Layers: []artifact.Descriptor{}})
	legacy := f.root("checkpoint-legacy", descriptor)
	legacy.ProtocolVersion = 1
	legacy.Graph[descriptor.Digest] = tasks.GraphEntry{Digest: descriptor.Digest, MediaType: descriptor.MediaType, Size: descriptor.Size}
	legacy.Checkpoints[descriptor.Digest] = "uploaded"
	saveLegacyAcceptanceTask(t, f, &legacy)
	intent := newReadyBlobIntent(t, ctx, f.service.tasks, f.service.records)
	root := newDemandRootTask(t, ctx, f.service.tasks, "checkpoint-terminal")
	linkRootBlobDemand(t, ctx, f.service.records, intent.ID, root.ID, strings.Repeat("2", 32), "scope", strings.Repeat("b", 32))
	if _, err := f.service.tasks.Cancel(ctx, root.ID); err != nil {
		t.Fatal(err)
	}
	phases := map[string]bool{}
	for units := 0; units < 500; units++ {
		r, work, release, err := f.service.openProtectionRecovery(ctx)
		if err != nil {
			t.Fatal(err)
		}
		phases[r.checkpoint.Phase] = true
		if err := r.step(work); err != nil {
			_ = release()
			t.Fatalf("phase %s/%s: %v", r.checkpoint.Phase, r.checkpoint.Step, err)
		}
		faults.putHook = func(key string, _ []byte) error {
			if key == protectionRecoveryKey {
				return storage.ErrUnavailable
			}
			return nil
		}
		if err := r.save(work); !errors.Is(err, storage.ErrUnavailable) {
			_ = release()
			t.Fatalf("checkpoint failure missing: %v", err)
		}
		faults.putHook = nil
		if err := release(); err != nil {
			t.Fatal(err)
		}
		replayed, work, release, err := f.service.openProtectionRecovery(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := replayed.step(work); err != nil {
			_ = release()
			t.Fatalf("replay %s/%s: %v", replayed.checkpoint.Phase, replayed.checkpoint.Step, err)
		}
		if err := replayed.save(work); err != nil {
			_ = release()
			t.Fatal(err)
		}
		if err := release(); err != nil {
			t.Fatal(err)
		}
		if replayed.checkpoint.Phase == "complete" {
			if f.service.gate.State().LegacyGrandfatheredBytes != descriptor.Size {
				t.Fatal("replayed legacy migration changed its reservation")
			}
			for _, phase := range []string{"root_demand_indexes", "cache_protection_index", "legacy_tasks", "legacy_link", "task_reconciliation", "task_reconciliation_blobs", "validate_final", "finalize"} {
				if !phases[phase] {
					t.Fatalf("fixture missed %s", phase)
				}
			}
			return
		}
	}
	t.Fatal("recovery did not converge after checkpoint replay")
}
