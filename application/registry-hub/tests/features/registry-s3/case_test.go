package registrys3

import "testing"

func TestFindDistributionMultipartUploadRequiresCompleteTwoPartLifecycle(t *testing.T) {
	logs := []byte("" +
		"owner bucket [13/Oct/2026:12:00:00 +0000] 10.0.0.1 requester create-1 s3_CreateMultipartUpload registry-hub-distribution/docker/registry/v2/repositories/repo/_uploads/id/data /bucket/key?uploads= 200 - 362 0 0 0\n" +
		"owner bucket [13/Oct/2026:12:00:01 +0000] 10.0.0.1 requester part-1 s3_UploadPart registry-hub-distribution/docker/registry/v2/repositories/repo/_uploads/id/data /bucket/key?partNumber=1&uploadId=upload-1 200 - 0 0 938 938\n" +
		"owner bucket [13/Oct/2026:12:00:02 +0000] 10.0.0.1 requester part-2 s3_UploadPart registry-hub-distribution/docker/registry/v2/repositories/repo/_uploads/id/data /bucket/key?partNumber=2&uploadId=upload-1 200 - 0 0 900 900\n" +
		"owner bucket [13/Oct/2026:12:00:03 +0000] 10.0.0.1 requester complete-1 s3_CompleteMultipartUpload registry-hub-distribution/docker/registry/v2/repositories/repo/_uploads/id/data /bucket/key?uploadId=upload-1 200 - 746 0 2 2\n")
	parts, err := findDistributionMultipartUpload(logs, "repo")
	if err != nil {
		t.Fatalf("findDistributionMultipartUpload: %v", err)
	}
	if parts != 2 {
		t.Fatalf("multipart parts = %d, want 2", parts)
	}
}

func TestFindDistributionMultipartUploadRejectsUnrelatedOrIncompleteRequests(t *testing.T) {
	logs := []byte("" +
		"owner bucket [13/Oct/2026:12:00:00 +0000] 10.0.0.1 requester create-1 s3_CreateMultipartUpload registry-hub-distribution/docker/registry/v2/repositories/other/_uploads/id/data /bucket/key?uploads= 200 - 362 0 0 0\n" +
		"owner bucket [13/Oct/2026:12:00:01 +0000] 10.0.0.1 requester part-1 s3_UploadPart registry-hub-distribution/docker/registry/v2/repositories/repo/_uploads/id/data /bucket/key?partNumber=1&uploadId=upload-1 200 - 0 0 938 938\n" +
		"owner bucket [13/Oct/2026:12:00:02 +0000] 10.0.0.1 requester part-2 s3_UploadPart registry-hub-distribution/docker/registry/v2/repositories/repo/_uploads/id/data /bucket/key?partNumber=2&uploadId=upload-1 500 XAmzContentSHA256Mismatch 0 0 900 900\n" +
		"owner bucket [13/Oct/2026:12:00:03 +0000] 10.0.0.1 requester complete-1 s3_CompleteMultipartUpload registry-hub-distribution/docker/registry/v2/repositories/repo/_uploads/id/data /bucket/key?uploadId=upload-2 200 - 746 0 2 2\n")
	if _, err := findDistributionMultipartUpload(logs, "repo"); err == nil {
		t.Fatal("findDistributionMultipartUpload accepted unrelated or incomplete requests")
	}
}

func TestVerifyRegistryObjectPrefixRejectsBackendState(t *testing.T) {
	if err := verifyRegistryObjectPrefix(map[string]string{
		"registry-hub-distribution/docker/registry/v2/blobs/sha256/aa/data": "etag:10",
	}); err != nil {
		t.Fatalf("verifyRegistryObjectPrefix: %v", err)
	}
	if err := verifyRegistryObjectPrefix(map[string]string{
		"registry-hub-distribution/tasks/state/task.json": "etag:10",
	}); err == nil {
		t.Fatal("verifyRegistryObjectPrefix accepted a backend task object")
	}
}

func TestCompareRegistryObjectSnapshotsRequiresStableObjects(t *testing.T) {
	before := map[string]string{"registry-hub-distribution/registry/config": "etag:10"}
	if err := compareRegistryObjectSnapshots(before, map[string]string{"registry-hub-distribution/registry/config": "etag:10"}); err != nil {
		t.Fatalf("compareRegistryObjectSnapshots: %v", err)
	}
	if err := compareRegistryObjectSnapshots(before, map[string]string{}); err == nil {
		t.Fatal("compareRegistryObjectSnapshots accepted a missing object")
	}
}
