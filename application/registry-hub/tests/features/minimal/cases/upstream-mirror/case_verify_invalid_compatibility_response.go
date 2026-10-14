package upstreammirror

import (
	"context"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"io"
	"net/http"
	"strings"
)

func verifyInvalidCompatibilityResponse(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, repository, upstreamRepository, discoveryDigest string) (map[string]any, error) {
	const reference = "minimal-compat-invalid"
	status, err := upstreamManifestStatusWithAccept(ctx, registry, repository, reference, support.DockerListMedia)
	if err != nil {
		return nil, err
	}
	if status != http.StatusNotAcceptable {
		return nil, fmt.Errorf("incompatible successful representation returned %d, want 406", status)
	}
	requests, err := countFixtureRequests(ctx, environment, reference)
	if err != nil {
		return nil, err
	}
	accepts, err := fixtureAcceptsForReference(ctx, environment, reference)
	if err != nil {
		return nil, err
	}
	if requests != 2 || len(accepts) != 2 || accepts[1] != support.DockerListMedia {
		return nil, fmt.Errorf("invalid compatibility response trace is invalid: requests=%d accepts=%q", requests, accepts)
	}
	task, err := waitForSolidifiedTask(ctx, environment, "github.com", upstreamRepository, reference)
	if err != nil {
		return nil, fmt.Errorf("valid discovery root did not solidify after invalid compatibility response: %w", err)
	}
	if testkit.String(task["rootDigest"]) != discoveryDigest || task["rootPublished"] != true {
		return nil, fmt.Errorf("invalid compatibility response changed or blocked the discovery root")
	}
	return map[string]any{
		"compatibilityInvalidStatus":         status,
		"compatibilityInvalidRequests":       requests,
		"compatibilityInvalidRootSolidified": true,
	}, nil
}

func verifyUpstreamRangeConditions(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, repository string, blob []byte, digest, etag string, start, end int) (map[string]any, error) {
	if etag == "" {
		return nil, fmt.Errorf("upstream range response has no ETag for If-Range checks")
	}
	invalid, err := registry.Get(ctx, repository, "/blobs/"+digest, map[string]string{"Range": "bytes=999999-"})
	if err != nil {
		return nil, err
	}
	invalidBody, readErr := io.ReadAll(invalid.Body)
	invalidStatus := invalid.StatusCode
	invalidContentRange := invalid.Header.Get("Content-Range")
	_ = invalid.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if invalidStatus != http.StatusRequestedRangeNotSatisfiable || invalidContentRange != fmt.Sprintf("bytes */%d", len(blob)) || len(invalidBody) == 0 {
		return nil, fmt.Errorf("invalid upstream-client range returned status=%d contentRange=%q", invalidStatus, invalidContentRange)
	}
	wrongValidator := `"sha256:` + strings.Repeat("0", 64) + `"`
	ignored, err := registry.Get(ctx, repository, "/blobs/"+digest, map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", start, end), "If-Range": wrongValidator})
	if err != nil {
		return nil, err
	}
	fullBody, readErr := io.ReadAll(ignored.Body)
	ignoredStatus := ignored.StatusCode
	_ = ignored.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if ignoredStatus != http.StatusOK || string(fullBody) != string(blob) {
		return nil, fmt.Errorf("If-Range mismatch returned status=%d or unexpected full bytes", ignoredStatus)
	}
	matched, err := registry.Get(ctx, repository, "/blobs/"+digest, map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", start, end), "If-Range": etag})
	if err != nil {
		return nil, err
	}
	partialBody, readErr := io.ReadAll(matched.Body)
	matchedStatus := matched.StatusCode
	matchedContentRange := matched.Header.Get("Content-Range")
	_ = matched.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	wantContentRange := fmt.Sprintf("bytes %d-%d/%d", start, end, len(blob))
	if matchedStatus != http.StatusPartialContent || matchedContentRange != wantContentRange || string(partialBody) != string(blob[start:end+1]) {
		return nil, fmt.Errorf("matching If-Range returned status=%d contentRange=%q or unexpected bytes", matchedStatus, matchedContentRange)
	}
	methods, err := fixtureMethodsForBlob(ctx, environment, digest)
	if err != nil {
		return nil, err
	}
	if len(methods) != 1 || methods[0] != http.MethodGet {
		return nil, fmt.Errorf("cached Range condition requests reached upstream: methods=%q", methods)
	}
	return map[string]any{
		"upstreamInvalidRangeStatus":       invalidStatus,
		"upstreamInvalidRangeContentRange": invalidContentRange,
		"upstreamInvalidRangeBodyPresent":  len(invalidBody) > 0,
		"upstreamIfRangeMismatchStatus":    ignoredStatus,
		"upstreamIfRangeMismatchFullBytes": string(fullBody) == string(blob),
		"upstreamIfRangeMatchStatus":       matchedStatus,
		"upstreamIfRangeMatchContentRange": matchedContentRange,
		"upstreamIfRangeMatchBytes":        string(partialBody) == string(blob[start:end+1]),
		"upstreamRangeConditionMethods":    methods,
	}, nil
}
