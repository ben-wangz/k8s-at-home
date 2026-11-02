package backends3

import "testing"

func TestCompareObjectSnapshotsAllowsIndexAndGCStateUpdates(t *testing.T) {
	before := map[string]string{
		"registry-hub-backend/tasks/state/task.json":                          "task-etag:10",
		"registry-hub-backend/cache/objects/object/representation-index.json": "index-etag:20",
		backendGCStateKey: "gc-etag:30",
	}
	after := map[string]string{
		"registry-hub-backend/tasks/state/task.json": "task-etag:10",
		backendGCStateKey: "gc-etag:31",
	}
	if err := compareObjectSnapshots(before, after); err != nil {
		t.Fatalf("compareObjectSnapshots: %v", err)
	}
}

func TestCompareObjectSnapshotsRejectsMissingPersistentObject(t *testing.T) {
	before := map[string]string{"registry-hub-backend/tasks/state/task.json": "task-etag:10"}
	if err := compareObjectSnapshots(before, nil); err == nil {
		t.Fatal("compareObjectSnapshots accepted a missing persistent object")
	}
}

func TestCompareObjectSnapshotsAllowsBackgroundProgressWithoutLosingCheckpoints(t *testing.T) {
	for _, relative := range []string{
		"background/v1/statistics-checkpoints/protection.json", "background/v1/statistics/tasks.json",
		"background/v1/maintenance-checkpoints/storage_inventory.json", "background/v1/queue-checkpoints/execution.json",
		"background/v1/source-checkpoints/source_tasks.json", "background/v1/scans/activation.json", "background/v1/status/notify.json",
		"diagnostics/v1/storage/checkpoint.json", "diagnostics/v1/workers/protection_recovery.json",
		"diagnostics/v1/event-budgets/20261102.json", "inventory/v1/checkpoint.json", "inventory/v1/lifecycle/root.json",
		"inventory/v1/projection-revisions/root.json", "system/gc/state.json", "downloads/v1/quota.json",
	} {
		t.Run(relative, func(t *testing.T) {
			key := backendS3Prefix + "/" + relative
			before := map[string]string{key: "before:10"}
			if err := compareObjectSnapshots(before, map[string]string{key: "after:20"}); err != nil {
				t.Fatalf("legitimate background progress rejected: %v", err)
			}
			if err := compareObjectSnapshots(before, nil); err == nil {
				t.Fatal("missing durable checkpoint accepted")
			}
		})
	}
}

func TestCompareObjectSnapshotsAllowsTransientCoordinationCleanup(t *testing.T) {
	before := map[string]string{
		backendS3Prefix + "/leases/background/queue/execution":         "lease:10",
		backendS3Prefix + "/background/v1/jobs/execution/job.json":     "job:10",
		backendS3Prefix + "/background/v1/receipts/execution/job.json": "receipt:10",
		backendS3Prefix + "/inventory/v1/changes/object.json":          "marker:10",
	}
	if err := compareObjectSnapshots(before, nil); err != nil {
		t.Fatalf("completed coordination state must be reclaimable: %v", err)
	}
}

func TestCompareObjectSnapshotsPreservesImmutablePayloadAndUnknownState(t *testing.T) {
	for _, relative := range []string{
		"cache/objects/blob/data", "tasks/state/task.json", "solidification/graphs/root/nodes/node.json",
		"inventory/v1/snapshots/report/entries/image.json", "diagnostics/v1/storage/snapshots/report/objects/00000000.json",
		"background/v1/unknown.json", "background/v1/statistics-checkpoints-archive/protection.json",
	} {
		t.Run(relative, func(t *testing.T) {
			key := backendS3Prefix + "/" + relative
			before := map[string]string{key: "before:10"}
			for name, after := range map[string]map[string]string{"missing": nil, "changed": {key: "after:20"}} {
				if err := compareObjectSnapshots(before, after); err == nil {
					t.Errorf("%s persistent object accepted", name)
				}
			}
		})
	}
}
