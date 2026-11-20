package diagnostics

import (
	"testing"
	"time"
)

func TestSeededUsageRunPrefixSeparatesStorageAndS3Keys(t *testing.T) {
	run := seededUsageRun{Stage: "reduce"}
	storageKey := seededUsageRunPrefix(diagnosticsStorageKeyPrefix, "fixture", run)
	if want := "diagnostics/v1/storage/snapshots/fixture/usage/reduce/0000/00000000/"; storageKey != want {
		t.Fatalf("storage key prefix = %q, want %q", storageKey, want)
	}
	objectKey := seededUsageRunPrefix(diagnosticsS3ObjectPrefix, "fixture", run)
	if want := "registry-hub-backend/diagnostics/v1/storage/snapshots/fixture/usage/reduce/0000/00000000/"; objectKey != want {
		t.Fatalf("S3 object prefix = %q, want %q", objectKey, want)
	}
}

func TestDiagnosticsS3ObjectKeyAddsConfiguredPrefix(t *testing.T) {
	storageKey := "diagnostics/v1/storage/snapshots/fixture/usage/reduce/0000/00000000.json"
	got, err := diagnosticsS3ObjectKey(storageKey)
	if err != nil {
		t.Fatal(err)
	}
	if want := "registry-hub-backend/" + storageKey; got != want {
		t.Fatalf("S3 object key = %q, want %q", got, want)
	}
	if _, err := diagnosticsS3ObjectKey("unexpected/key"); err == nil {
		t.Fatal("key without storage prefix was accepted")
	}
}

func TestSeededSourceSweepRetryIdentityTracksSourceVersion(t *testing.T) {
	first, err := seededSourceSweepRetryID("receipt_cleanup", "default", "background/v1/receipts/test/one.json", "etag-one")
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := seededSourceSweepRetryID("receipt_cleanup", "default", "background/v1/receipts/test/one.json", "etag-one")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := seededSourceSweepRetryID("receipt_cleanup", "default", "background/v1/receipts/test/one.json", "etag-two")
	if err != nil {
		t.Fatal(err)
	}
	if first != repeated || first == updated {
		t.Fatalf("retry IDs = %q, %q, %q; want stable ID for one source version and a new ID after version change", first, repeated, updated)
	}
}

func TestSeededSourceSweepRetryDueSuffixUsesSortableTimestampAndRevision(t *testing.T) {
	dueAt := time.Date(2026, time.November, 20, 12, 34, 56, 123456789, time.FixedZone("UTC+1", 3600))
	if got, want := seededSourceSweepRetryDueSuffix(dueAt, "retry-id", 7), "20261120T113456.123456789Z/retry-id-00000000000000000007.json"; got != want {
		t.Fatalf("due suffix = %q, want %q", got, want)
	}
}
