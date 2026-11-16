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

func TestCompareObjectSnapshotsAllowsWholeReclaimableCacheIdentity(t *testing.T) {
	dataKey := backendS3Prefix + "/cache/objects/blob/data"
	prefix := backendS3Prefix + "/cache/objects/blob/"
	before := map[string]string{
		dataKey:                                    "data:10",
		prefix + "complete/type.json":              "complete:20",
		prefix + "representation-index.json":       "index:30",
		prefix + "representations/type.json":       "metadata:40",
		backendS3Prefix + "/tasks/state/task.json": "task:50",
	}
	after := map[string]string{backendS3Prefix + "/tasks/state/task.json": "task:50"}
	if err := compareObjectSnapshots(before, after, dataKey); err != nil {
		t.Fatalf("compareObjectSnapshots accepted whole cache identity reclamation: %v", err)
	}

	partial := map[string]string{
		dataKey:                                    "data:10",
		prefix + "complete/type.json":              "complete:20",
		prefix + "representation-index.json":       "index:30",
		backendS3Prefix + "/tasks/state/task.json": "task:50",
	}
	if err := compareObjectSnapshots(before, partial, dataKey); err == nil {
		t.Fatal("compareObjectSnapshots accepted partial cache identity reclamation")
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
		"inventory/v1/latest-work/root.json", "inventory/v1/retention.json", "inventory/v1/work-retention.json",
		"background/v1/quota-signal.json",
		"system/recovery/protection.json", "tasks/state/task.json", "tasks/dispatch/v1/repair.json",
		"solidification/blobs/intents/intent.json",
		"system/solidification/sessions/session.json",
		"inventory/v1/fragment-retention.json", "inventory/v1/fragments/image/image.json", "inventory/v1/fragments/content/content.json",
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
		backendS3Prefix + "/tasks/wake/task.json":                      "wake:10",
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
		"cache/objects/blob/data", "solidification/graphs/root/nodes/node.json",
		"inventory/v1/snapshots/report/entries/image.json", "diagnostics/v1/storage/snapshots/report/objects/00000000.json",
		"background/v1/unknown.json", "background/v1/statistics-checkpoints-archive/protection.json",
		"inventory/v1/fragments/unknown/object.json", "inventory/v1/fragments/image-archive/image.json",
		"solidification/blobs/intents-archive/intent.json", "solidification/blobs/index/blob.json",
		"system/solidification/sessions-archive/session.json", "system/solidification/owners/set/owner.json",
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

func TestCompareObjectSnapshotsDistinguishesStagingAndPublishedAssociations(t *testing.T) {
	prefix := backendS3Prefix + "/diagnostics/v1/storage/snapshots/report/"
	for _, domain := range []string{"state", "registry"} {
		key := prefix + "associations/" + domain + "/object.json"
		before, after := map[string]string{key: "before:10"}, map[string]string{key: "after:20"}
		if err := compareObjectSnapshots(before, after); err != nil {
			t.Fatal("unpublished attribution could not resume:", err)
		}
		if err := compareObjectSnapshots(before, nil); err == nil {
			t.Fatal("missing staging association accepted")
		}
		before[prefix+"manifest.json"], after[prefix+"manifest.json"] = "manifest:30", "manifest:30"
		if err := compareObjectSnapshots(before, after); err == nil {
			t.Fatal("published association mutation accepted")
		}
	}
	for _, suffix := range []string{"objects/00000000.json", "associations/unknown/object.json", "associations/state/archive/object.json"} {
		key := prefix + suffix
		if err := compareObjectSnapshots(map[string]string{key: "before:10"}, map[string]string{key: "after:20"}); err == nil {
			t.Fatalf("immutable or unknown staging object mutation accepted: %s", suffix)
		}
	}
}
