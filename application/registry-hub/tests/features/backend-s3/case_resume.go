package backends3

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

const resumeFixtureRepository = "github.com/registry-hub-test/fixture"

func verifyBackendS3Resume(ctx context.Context, environment testkit.CaseContext, client *s3.Client, bucket string, registry *support.RegistryClient) (map[string]any, error) {
	expected, digest := support.BuildResumeLayerFixture("s3")
	started := time.Now()
	response, err := registry.Get(ctx, resumeFixtureRepository, "/blobs/"+digest, nil)
	if err != nil {
		return nil, err
	}
	firstStatus := response.StatusCode
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if firstStatus == http.StatusOK {
		return nil, fmt.Errorf("interrupted S3 download unexpectedly completed on its first request")
	}
	prefix := resumeStatePrefix(digest)
	partial, err := listBackendObjects(ctx, client, bucket)
	if err != nil {
		return nil, err
	}
	stateKey := prefix + "state.json"
	chunks := countObjectPath(partial, prefix, "/chunks/")
	if _, exists := partial[stateKey]; !exists || chunks == 0 {
		return nil, fmt.Errorf("interrupted download has no persisted S3 state or chunks: state=%t chunks=%d", partial[stateKey] != "", chunks)
	}
	blob, err := support.ReadBlob(ctx, registry, resumeFixtureRepository, digest)
	if err != nil || len(blob) != len(expected) || support.Digest(blob) != digest {
		return nil, fmt.Errorf("S3-resumed blob failed size or digest verification: err=%v size=%d", err, len(blob))
	}
	blobTask, err := waitForBlobTask(ctx, environment, digest)
	if err != nil {
		return nil, err
	}
	after, err := listBackendObjects(ctx, client, bucket)
	if err != nil {
		return nil, err
	}
	if countObjectPrefix(after, prefix) != 0 {
		return nil, fmt.Errorf("completed S3 download retained temporary state objects")
	}
	cacheIdentity := strings.TrimPrefix(support.Digest([]byte("github.com\x00"+"registry-hub-test/fixture\x00"+digest)), "sha256:")
	cacheKey := backendS3Prefix + "/cache/objects/" + cacheIdentity + "/data"
	if _, exists := after[cacheKey]; !exists {
		return nil, fmt.Errorf("completed S3 blob is missing its full cache object")
	}
	cacheObject := after[cacheKey]
	separator := strings.LastIndexByte(cacheObject, ':')
	if separator < 0 {
		return nil, fmt.Errorf("completed S3 cache object metadata is invalid")
	}
	multipartETag := strings.Trim(cacheObject[:separator], `"`)
	storedSize, err := strconv.ParseInt(cacheObject[separator+1:], 10, 64)
	if err != nil || storedSize != int64(len(blob)) || storedSize <= 5<<20 {
		return nil, fmt.Errorf("completed S3 cache object size is %d, want %d bytes above 5 MiB", storedSize, len(blob))
	}
	multipartParts, isMultipart := testkit.S3MultipartPartCount(multipartETag)
	if !isMultipart || multipartParts != 2 {
		return nil, fmt.Errorf("resumed S3 cache object ETag %q identifies %d parts, want 2", multipartETag, multipartParts)
	}
	lines, err := resumeFixtureLogs(ctx, environment, started)
	if err != nil {
		return nil, err
	}
	offsets := resumeRangeOffsets(lines)
	if len(offsets) != 5 || !sameOffsets(offsets, 1<<20) || countLogStatus(lines, "status=503") != 4 {
		return nil, fmt.Errorf("S3 retry did not resume the persisted offset: ranges=%v 503s=%d", offsets, countLogStatus(lines, "status=503"))
	}
	return map[string]any{
		"digest": digest, "size": len(blob), "firstAttemptStatus": firstStatus,
		"blobTaskID": testkit.String(blobTask["id"]), "blobTaskStatus": testkit.String(blobTask["status"]),
		"stateKey": stateKey, "partialChunkCount": chunks, "temporaryObjectsAfterCompletion": countObjectPrefix(after, prefix),
		"cacheObjectKey": cacheKey, "rangeOffsets": offsets, "status503": countLogStatus(lines, "status=503"),
		"multipartObjectSize": storedSize, "multipartETag": multipartETag, "multipartParts": multipartParts,
	}, nil
}

func waitForBlobTask(ctx context.Context, environment testkit.CaseContext, digest string) (map[string]any, error) {
	query := url.Values{
		"authority":  {"github.com"},
		"repository": {"registry-hub-test/fixture"},
		"limit":      {"1000"},
	}
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	for {
		response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks?"+query.Encode(), nil, nil)
		if err != nil {
			return nil, err
		}
		var page map[string]any
		if _, err := support.DecodeJSON(response, 4<<20, &page); err != nil {
			return nil, err
		}
		for _, item := range testkit.Slice(page["items"]) {
			task := testkit.Map(item)
			if testkit.String(task["kind"]) != "blob" || testkit.String(task["blobDigest"]) != digest {
				continue
			}
			status := testkit.String(task["status"])
			if status == "verified_local" || status == "solidified" {
				return task, nil
			}
			if status == "failed" || status == "cancelled" {
				return nil, fmt.Errorf("S3-resumed blob task ended in %s (%s)", status, testkit.String(task["errorClass"]))
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("S3-resumed blob task did not reach verified_local")
		case <-time.After(time.Second):
		}
	}
}

func resumeStatePrefix(digest string) string {
	identity := strings.TrimPrefix(support.Digest([]byte("github.com\x00"+"registry-hub-test/fixture\x00"+digest)), "sha256:")
	state := strings.TrimPrefix(support.Digest([]byte(identity+"\x00"+digest)), "sha256:")
	return backendS3Prefix + "/downloads/v1/" + state + "/"
}

func countObjectPrefix(objects map[string]string, prefix string) int {
	count := 0
	for key := range objects {
		if strings.HasPrefix(key, prefix) {
			count++
		}
	}
	return count
}

func countObjectPath(objects map[string]string, prefix, marker string) int {
	count := 0
	for key := range objects {
		if strings.HasPrefix(key, prefix) && strings.Contains(key, marker) {
			count++
		}
	}
	return count
}

func resumeFixtureLogs(ctx context.Context, environment testkit.CaseContext, since time.Time) ([]string, error) {
	result, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace,
		"logs", "pod/"+environment.Release+"-external-upstream-proxy", "-c", "proxy",
		"--since-time="+since.UTC().Format(time.RFC3339Nano), "--tail=20000")
	if err != nil {
		return nil, err
	}
	marker := "resume_fixture reference=resume-s3 "
	var lines []string
	for _, line := range strings.Split(string(result.Stdout), "\n") {
		if strings.Contains(line, marker) {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

func resumeRangeOffsets(lines []string) []int64 {
	var offsets []int64
	for _, line := range lines {
		marker := `range="bytes=`
		start := strings.Index(line, marker)
		if start < 0 {
			continue
		}
		value := line[start+len(marker):]
		end := strings.IndexByte(value, '-')
		if end < 0 {
			continue
		}
		offset, err := strconv.ParseInt(value[:end], 10, 64)
		if err == nil {
			offsets = append(offsets, offset)
		}
	}
	return offsets
}

func sameOffsets(offsets []int64, expected int64) bool {
	if len(offsets) == 0 {
		return false
	}
	for _, offset := range offsets {
		if offset != expected {
			return false
		}
	}
	return true
}

func countLogStatus(lines []string, status string) int {
	count := 0
	for _, line := range lines {
		if strings.Contains(line, status) {
			count++
		}
	}
	return count
}
