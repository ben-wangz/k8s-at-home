package upstreammirror

import (
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"io"
	"net/http"
	"strings"
)

func verifyClientAcceptAndHead(run *mirrorRun) (map[string]any, error) {
	first, err := upstreamManifestStatusWithAccept(run.context, run.registry, run.remoteRepository, "minimal-client-incompatible", "text/plain")
	if err != nil {
		return nil, err
	}
	second, err := upstreamManifestStatusWithAccept(run.context, run.registry, run.remoteRepository, "minimal-client-incompatible", "text/plain")
	if err != nil {
		return nil, err
	}
	if first != http.StatusNotAcceptable || second != http.StatusNotAcceptable {
		return nil, fmt.Errorf("unsupported client Accept statuses = %d, %d; want 406 twice", first, second)
	}
	requests, err := countFixtureRequests(run.context, run.environment, "minimal-client-incompatible")
	if err != nil {
		return nil, err
	}
	if requests != 0 {
		return nil, fmt.Errorf("unsupported client Accept reached upstream %d times, want 0", requests)
	}
	tasks, err := countTasksForReference(run.context, run.environment, "github.com", run.upstreamRepository, "minimal-client-incompatible")
	if err != nil {
		return nil, err
	}
	if tasks != 0 {
		return nil, fmt.Errorf("unsupported client Accept created %d tasks, want 0", tasks)
	}
	response, err := run.registry.Head(run.context, run.remoteRepository, "/manifests/minimal-head-405", map[string]string{"Accept": support.OCIManifestMediaType})
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(response.Body)
	status := response.StatusCode
	_ = response.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if status != http.StatusMethodNotAllowed || len(body) != 0 {
		return nil, fmt.Errorf("upstream HEAD 405 returned status %d with %d body bytes", status, len(body))
	}
	methods, err := fixtureMethodsForReference(run.context, run.environment, "minimal-head-405")
	if err != nil {
		return nil, err
	}
	if len(methods) != 1 || methods[0] != http.MethodHead {
		return nil, fmt.Errorf("upstream HEAD methods = %q, want one HEAD and no GET fallback", methods)
	}
	headTasks, err := countTasksForReference(run.context, run.environment, "github.com", run.upstreamRepository, "minimal-head-405")
	if err != nil {
		return nil, err
	}
	if headTasks != 0 {
		return nil, fmt.Errorf("upstream HEAD created %d solidification tasks, want 0", headTasks)
	}
	return map[string]any{
		"clientIncompatibleFirstStatus": first, "clientIncompatibleSecondStatus": second,
		"clientIncompatibleRequests": requests, "clientIncompatibleTasks": tasks,
		"upstreamHeadStatus": status, "upstreamHeadMethods": methods,
		"upstreamHeadBodyBytes": len(body), "upstreamHeadTasks": headTasks,
	}, nil
}

func verifyUpstreamRangeRequest(run *mirrorRun) (map[string]any, error) {
	blob := []byte("upstream-range-fixture")
	digest, start, end := support.Digest(blob), 2, 7
	response, err := run.registry.Get(run.context, run.remoteRepository, "/blobs/"+digest, map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", start, end)})
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(response.Body)
	status := response.StatusCode
	contentRange := response.Header.Get("Content-Range")
	responseDigest := response.Header.Get("Docker-Content-Digest")
	etag := response.Header.Get("ETag")
	_ = response.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	wantRange := fmt.Sprintf("bytes %d-%d/%d", start, end, len(blob))
	if status != http.StatusPartialContent || contentRange != wantRange || responseDigest != digest || string(body) != string(blob[start:end+1]) {
		return nil, fmt.Errorf("upstream ranged blob response has status %d, range %q, digest %q, or unexpected bytes", status, contentRange, responseDigest)
	}
	methods, err := fixtureMethodsForBlob(run.context, run.environment, digest)
	if err != nil {
		return nil, err
	}
	if len(methods) != 1 || methods[0] != http.MethodGet {
		return nil, fmt.Errorf("ranged blob upstream methods = %q, want one complete GET", methods)
	}
	ranges, err := fixtureRangeHeadersForBlob(run.context, run.environment, digest)
	if err != nil {
		return nil, err
	}
	if len(ranges) != 1 || ranges[0] != "" {
		return nil, fmt.Errorf("ranged blob upstream Range headers = %q, want one empty header", ranges)
	}
	conditions, err := verifyUpstreamRangeConditions(run.context, run.environment, run.registry, run.remoteRepository, blob, digest, etag, start, end)
	if err != nil {
		return nil, err
	}
	result := map[string]any{
		"upstreamRangeStatus": status, "upstreamRangeContentRange": contentRange,
		"upstreamRangeDigest": responseDigest, "upstreamRangeBytesMatch": string(body) == string(blob[start:end+1]),
		"upstreamRangeMethods": methods, "upstreamRangeHeaders": ranges,
	}
	mergeMirrorResults(result, conditions)
	return result, nil
}

func verifyDigestAndMissBehavior(run *mirrorRun) (map[string]any, error) {
	digestReference := "sha256:" + strings.Repeat("0", 64)
	firstStatus, err := upstreamManifestStatus(run.context, run.registry, run.remoteRepository, digestReference)
	if err != nil {
		return nil, err
	}
	if firstStatus != http.StatusBadGateway {
		return nil, fmt.Errorf("manifest response for digest reference returned %d, want 502", firstStatus)
	}
	firstRequests, err := countFixtureRequests(run.context, run.environment, digestReference)
	if err != nil {
		return nil, err
	}
	if firstRequests != 1 {
		return nil, fmt.Errorf("digest mismatch first request count = %d, want 1", firstRequests)
	}
	tasks, err := tasksForReference(run.context, run.environment, "github.com", run.upstreamRepository, digestReference)
	if err != nil {
		return nil, err
	}
	if len(tasks) != 1 {
		return nil, fmt.Errorf("digest mismatch created %d tasks, want one failed task", len(tasks))
	}
	for _, item := range tasks {
		task := testkit.Map(item)
		if testkit.String(task["status"]) != "failed" || testkit.String(task["rootDigest"]) != "" || task["rootPublished"] == true {
			return nil, fmt.Errorf("digest mismatch task published a root or did not fail: status=%q root=%q", testkit.String(task["status"]), testkit.String(task["rootDigest"]))
		}
	}
	secondStatus, err := upstreamManifestStatus(run.context, run.registry, run.remoteRepository, digestReference)
	if err != nil {
		return nil, err
	}
	if secondStatus != http.StatusBadGateway {
		return nil, fmt.Errorf("repeated digest mismatch returned %d, want 502", secondStatus)
	}
	secondRequests, err := countFixtureRequests(run.context, run.environment, digestReference)
	if err != nil {
		return nil, err
	}
	if secondRequests <= firstRequests {
		return nil, fmt.Errorf("digest mismatch was cached: request count stayed at %d", firstRequests)
	}
	missingStatus, err := upstreamManifestStatus(run.context, run.registry, run.remoteRepository, "minimal-missing")
	if err != nil {
		return nil, err
	}
	if missingStatus != http.StatusNotFound {
		return nil, fmt.Errorf("confirmed upstream 404 returned %d", missingStatus)
	}
	logs, err := fixtureLogs(run.context, run.environment)
	if err != nil {
		return nil, err
	}
	missingMarker := "host=github.com path=/v2/registry-hub-test/fixture/manifests/minimal-missing auth=absent"
	if strings.Count(logs, missingMarker) != 1 {
		return nil, fmt.Errorf("confirmed upstream 404 requests = %d, want 1", strings.Count(logs, missingMarker))
	}
	tagResults, err := verifyUpstreamTagPagination(run.context, run.environment, run.registry)
	if err != nil {
		return nil, err
	}
	unavailable, err := verifyUnavailableRetry(run)
	if err != nil {
		return nil, err
	}
	result := map[string]any{
		"digestMismatchFirstStatus": firstStatus, "digestMismatchFirstRequests": firstRequests,
		"digestMismatchSecondStatus": secondStatus, "digestMismatchSecondRequests": secondRequests,
		"digestMismatchTaskCount": len(tasks), "digestMismatchRootUnpublished": true,
		"confirmed404Status": missingStatus, "confirmed404RequestCount": 1,
		"upstreamTags": tagResults,
	}
	mergeMirrorResults(result, unavailable)
	return result, nil
}

func verifyUnavailableRetry(run *mirrorRun) (map[string]any, error) {
	firstStatus, err := upstreamManifestStatus(run.context, run.registry, run.remoteRepository, "minimal-unavailable")
	if err != nil {
		return nil, err
	}
	if firstStatus < http.StatusInternalServerError || firstStatus == http.StatusNotFound {
		return nil, fmt.Errorf("upstream 503 was misclassified as a miss: returned %d", firstStatus)
	}
	firstCount, err := countFixtureRequests(run.context, run.environment, "minimal-unavailable")
	if err != nil {
		return nil, err
	}
	secondStatus, err := upstreamManifestStatus(run.context, run.registry, run.remoteRepository, "minimal-unavailable")
	if err != nil {
		return nil, err
	}
	if secondStatus < http.StatusInternalServerError || secondStatus == http.StatusNotFound {
		return nil, fmt.Errorf("repeated upstream 503 was misclassified as a miss: returned %d", secondStatus)
	}
	secondCount, err := countFixtureRequests(run.context, run.environment, "minimal-unavailable")
	if err != nil {
		return nil, err
	}
	if secondCount <= firstCount {
		return nil, fmt.Errorf("upstream failure was cached: request count stayed at %d", firstCount)
	}
	return map[string]any{
		"upstream503FirstStatus": firstStatus, "upstream503SecondStatus": secondStatus,
		"upstream503AttemptsFirst": firstCount, "upstream503AttemptsSecond": secondCount,
	}, nil
}
