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
