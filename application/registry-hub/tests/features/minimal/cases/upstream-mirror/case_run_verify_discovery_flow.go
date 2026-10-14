package upstreammirror

import (
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"mime"
	"net/http"
	"strings"
	"time"
)

func verifyDiscoveryFlow(run *mirrorRun) (map[string]any, string, error) {
	registryLogSince := time.Now().UTC().Add(-5 * time.Second)
	discoveryBody, headers, err := support.ReadManifest(run.context, run.registry, run.remoteRepository, "minimal-discovery", support.DockerListMedia)
	if err != nil {
		return nil, "", fmt.Errorf("Docker manifest list compatibility read failed: %w", err)
	}
	mediaType, _, err := mime.ParseMediaType(headers.Get("Content-Type"))
	if err != nil || mediaType != support.DockerListMedia || string(discoveryBody) != dockerListBody {
		return nil, "", fmt.Errorf("Docker manifest list compatibility response has unexpected media type or bytes")
	}
	discoveryDigest := support.Digest(discoveryBody)
	if headers.Get("Docker-Content-Digest") != discoveryDigest {
		return nil, "", fmt.Errorf("Docker manifest list compatibility response digest does not match its bytes")
	}
	task, err := waitForSolidifiedTask(run.context, run.environment, "github.com", run.upstreamRepository, "minimal-discovery")
	if err != nil {
		return nil, "", err
	}
	childManifest, indexBody, err := discoveryIndexFixture()
	if err != nil {
		return nil, "", err
	}
	indexDigest := support.Digest(indexBody)
	if testkit.String(task["rootDigest"]) != indexDigest || testkit.String(task["mediaType"]) != support.OCIIndexMediaType || testkit.String(task["status"]) != "solidified" || task["rootPublished"] != true {
		return nil, "", fmt.Errorf("compatibility response replaced the original OCI index discovery root")
	}
	childDigest := support.Digest(childManifest)
	configDigest := support.Digest([]byte("{}"))
	logLines, err := registryLogLines(run.context, run.environment, registryLogSince)
	if err != nil {
		return nil, "", err
	}
	publishResults, err := verifyPublishTopology(logLines, childDigest, indexDigest, configDigest)
	if err != nil {
		return nil, "", err
	}
	if err := verifyDiscoveryIndexFields(indexBody); err != nil {
		return nil, "", err
	}
	graph, checkpoints := testkit.Map(task["graph"]), testkit.Map(task["checkpoints"])
	if err := verifyDiscoveryGraph(graph, checkpoints, indexDigest, indexBody, childDigest, childManifest, configDigest); err != nil {
		return nil, "", err
	}
	localIndexBody, localIndexHeaders, err := support.ReadManifest(run.context, run.registry, run.remoteRepository, indexDigest, support.OCIIndexMediaType)
	if err != nil {
		return nil, "", fmt.Errorf("solidified OCI index is not locally readable: %w", err)
	}
	localIndexType, _, err := mime.ParseMediaType(localIndexHeaders.Get("Content-Type"))
	if err != nil || localIndexType != support.OCIIndexMediaType || string(localIndexBody) != string(indexBody) || localIndexHeaders.Get("Docker-Content-Digest") != indexDigest {
		return nil, "", fmt.Errorf("solidified OCI index media type or raw bytes changed")
	}
	localChildBody, localChildHeaders, err := support.ReadManifest(run.context, run.registry, run.remoteRepository, childDigest, support.OCIManifestMediaType)
	if err != nil {
		return nil, "", fmt.Errorf("solidified OCI child manifest is not locally readable: %w", err)
	}
	localChildType, _, err := mime.ParseMediaType(localChildHeaders.Get("Content-Type"))
	if err != nil || localChildType != support.OCIManifestMediaType || string(localChildBody) != string(childManifest) || localChildHeaders.Get("Docker-Content-Digest") != childDigest {
		return nil, "", fmt.Errorf("solidified OCI child media type or raw bytes changed")
	}
	platform := testkit.Map(testkit.Map(graph[childDigest])["platform"])
	if testkit.String(platform["architecture"]) != "amd64" || testkit.String(platform["os"]) != "linux" {
		return nil, "", fmt.Errorf("upstream OCI index child platform metadata changed")
	}
	requests, accepts, err := verifyDiscoveryRequestTrace(run)
	if err != nil {
		return nil, "", err
	}
	result := map[string]any{
		"discoveryRootDigest": indexDigest, "discoveryRootMediaType": testkit.String(task["mediaType"]),
		"discoveryGraphObjects": len(graph), "discoveryGraphCheckpoints": len(checkpoints),
		"discoveryChildPlatform": platform, "discoveryIndexMediaType": localIndexType,
		"discoveryIndexRawBytesPreserved": string(indexBody) == string(localIndexBody),
		"discoveryChildMediaType":         localChildType, "discoveryChildRawBytesPreserved": string(childManifest) == string(localChildBody),
		"compatibleDockerDigest": discoveryDigest, "compatibleDockerBytesMatch": string(discoveryBody) == dockerListBody,
		"discoveryRequests": requests, "discoveryAccepts": accepts,
	}
	mergeMirrorResults(result, publishResults)
	return result, indexDigest, nil
}

func verifyDiscoveryIndexFields(indexBody []byte) error {
	var fields map[string]any
	if err := json.Unmarshal(indexBody, &fields); err != nil {
		return fmt.Errorf("decode discovery index fixture fields: %w", err)
	}
	descriptors := testkit.Slice(fields["manifests"])
	if len(descriptors) != 1 {
		return fmt.Errorf("discovery OCI index has %d descriptors, want one", len(descriptors))
	}
	descriptor := testkit.Map(descriptors[0])
	platform := testkit.Map(descriptor["platform"])
	if testkit.String(platform["variant"]) != "v3" || testkit.String(platform["os.version"]) != "fixture-1" || len(testkit.Slice(platform["os.features"])) != 1 || testkit.String(testkit.Slice(platform["os.features"])[0]) != "fixture-feature" || testkit.String(testkit.Map(descriptor["annotations"])["org.opencontainers.image.ref.name"]) != "fixture-child" || testkit.String(descriptor["x-fixture-descriptor"]) != "retained" {
		return fmt.Errorf("discovery OCI index fixture omits platform or descriptor extension fields")
	}
	return nil
}

func verifyDiscoveryGraph(graph, checkpoints map[string]any, indexDigest string, indexBody []byte, childDigest string, childManifest []byte, configDigest string) error {
	expected := map[string]struct {
		mediaType string
		size      int
	}{
		indexDigest:  {mediaType: support.OCIIndexMediaType, size: len(indexBody)},
		childDigest:  {mediaType: support.OCIManifestMediaType, size: len(childManifest)},
		configDigest: {mediaType: "application/vnd.oci.image.config.v1+json", size: len([]byte("{}"))},
	}
	if len(graph) != len(expected) || len(checkpoints) != len(expected) {
		return fmt.Errorf("upstream OCI index graph has %d nodes and %d checkpoints, want %d each", len(graph), len(checkpoints), len(expected))
	}
	for digest, object := range expected {
		entry := testkit.Map(graph[digest])
		if testkit.String(entry["digest"]) != digest || testkit.String(entry["mediaType"]) != object.mediaType || testkit.Int(entry["size"]) != int64(object.size) {
			return fmt.Errorf("upstream OCI index graph entry %q has invalid digest, media type, or size", digest)
		}
		if testkit.String(checkpoints[digest]) != "verified" {
			return fmt.Errorf("upstream OCI index graph entry %q has no verified checkpoint", digest)
		}
	}
	return nil
}

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
