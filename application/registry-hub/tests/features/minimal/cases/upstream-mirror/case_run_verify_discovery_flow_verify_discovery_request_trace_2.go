package upstreammirror

import (
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"strings"
)

func verifyDiscoveryRequestTrace(run *mirrorRun) (int, []string, error) {
	requests, err := countFixtureRequests(run.context, run.environment, "minimal-discovery")
	if err != nil {
		return 0, nil, err
	}
	if requests != 2 {
		return 0, nil, fmt.Errorf("manifest discovery and compatibility request count = %d, want 2", requests)
	}
	accepts, err := fixtureAcceptsForReference(run.context, run.environment, "minimal-discovery")
	if err != nil {
		return 0, nil, err
	}
	if len(accepts) != 2 {
		return 0, nil, fmt.Errorf("manifest discovery Accept trace has %d requests, want 2", len(accepts))
	}
	for _, mediaType := range []string{support.OCIIndexMediaType, support.DockerListMedia, support.OCIManifestMediaType, support.DockerManifestMedia} {
		if !strings.Contains(accepts[0], mediaType) {
			return 0, nil, fmt.Errorf("initial discovery Accept %q omits %q", accepts[0], mediaType)
		}
	}
	if accepts[1] != support.DockerListMedia {
		return 0, nil, fmt.Errorf("compatibility Accept = %q, want only %q", accepts[1], support.DockerListMedia)
	}
	return requests, accepts, nil
}

func verifyCompatibilityFallbacks(run *mirrorRun, indexDigest string) (map[string]any, error) {
	status, err := upstreamManifestStatusWithAccept(run.context, run.registry, run.remoteRepository, "minimal-compat-unavailable", support.DockerListMedia)
	if err != nil {
		return nil, err
	}
	if status != http.StatusNotAcceptable {
		return nil, fmt.Errorf("upstream compatibility failure returned %d, want 406", status)
	}
	requests, err := countFixtureRequests(run.context, run.environment, "minimal-compat-unavailable")
	if err != nil {
		return nil, err
	}
	if requests != 2 {
		return nil, fmt.Errorf("failed compatibility request count = %d, want discovery and one compatibility request", requests)
	}
	accepts, err := fixtureAcceptsForReference(run.context, run.environment, "minimal-compat-unavailable")
	if err != nil {
		return nil, err
	}
	if len(accepts) != 2 || accepts[1] != support.DockerListMedia {
		return nil, fmt.Errorf("failed compatibility request Accept trace is invalid: %q", accepts)
	}
	task, err := waitForSolidifiedTask(run.context, run.environment, "github.com", run.upstreamRepository, "minimal-compat-unavailable")
	if err != nil {
		return nil, fmt.Errorf("valid discovery root did not solidify after compatibility failure: %w", err)
	}
	if testkit.String(task["rootDigest"]) != indexDigest || task["rootPublished"] != true {
		return nil, fmt.Errorf("compatibility failure changed or blocked the valid discovery root")
	}
	status503, err := upstreamManifestStatusWithAccept(run.context, run.registry, run.remoteRepository, "minimal-compat-503", support.DockerListMedia)
	if err != nil {
		return nil, err
	}
	if status503 != http.StatusBadGateway {
		return nil, fmt.Errorf("upstream compatibility 503 returned %d, want 502", status503)
	}
	requests503, err := countFixtureRequests(run.context, run.environment, "minimal-compat-503")
	if err != nil {
		return nil, err
	}
	accepts503, err := fixtureAcceptsForReference(run.context, run.environment, "minimal-compat-503")
	if err != nil {
		return nil, err
	}
	if requests503 < 2 || len(accepts503) != requests503 || !strings.Contains(accepts503[0], support.OCIIndexMediaType) {
		return nil, fmt.Errorf("compatibility 503 fixture trace is invalid: requests=%d accepts=%q", requests503, accepts503)
	}
	for _, accept := range accepts503[1:] {
		if accept != support.DockerListMedia {
			return nil, fmt.Errorf("compatibility 503 retry Accept = %q, want only %q", accept, support.DockerListMedia)
		}
	}
	task503, err := waitForSolidifiedTask(run.context, run.environment, "github.com", run.upstreamRepository, "minimal-compat-503")
	if err != nil {
		return nil, fmt.Errorf("valid discovery root did not solidify after compatibility 503: %w", err)
	}
	if testkit.String(task503["rootDigest"]) != indexDigest || task503["rootPublished"] != true {
		return nil, fmt.Errorf("compatibility 503 changed or blocked the valid discovery root")
	}
	invalid, err := verifyInvalidCompatibilityResponse(run.context, run.environment, run.registry, run.remoteRepository, run.upstreamRepository, indexDigest)
	if err != nil {
		return nil, err
	}
	result := map[string]any{
		"compatibilityFailureStatus": status, "compatibilityFailureRequests": requests,
		"compatibilityFailureRootSolidified": true, "compatibility503Status": status503,
		"compatibility503Requests": requests503, "compatibility503RootSolidified": true,
	}
	mergeMirrorResults(result, invalid)
	return result, nil
}
