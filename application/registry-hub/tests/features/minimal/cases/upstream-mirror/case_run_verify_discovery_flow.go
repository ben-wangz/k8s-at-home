package upstreammirror

import (
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"mime"
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
	childManifest, indexBody, err := discoveryIndexFixture()
	if err != nil {
		return nil, "", err
	}
	indexDigest := support.Digest(indexBody)
	childDigest := support.Digest(childManifest)
	config := []byte(discoveryConfigBody)
	configDigest := support.Digest(config)
	task, err := waitForTaskStatus(run.context, run.environment, "github.com", run.upstreamRepository, "minimal-discovery", "waiting_dependencies")
	if err != nil {
		return nil, "", fmt.Errorf("manifest discovery did not wait for client blob demand: %w", err)
	}
	configRequestsBefore, err := countFixturePath(run.context, run.environment, "/v2/registry-hub-test/fixture/blobs/"+configDigest)
	if err != nil {
		return nil, "", err
	}
	if configRequestsBefore != 0 {
		return nil, "", fmt.Errorf("manifest discovery fetched an unrequested config blob %d times", configRequestsBefore)
	}
	clientConfig, err := support.ReadBlob(run.context, run.registry, run.remoteRepository, configDigest)
	if err != nil {
		return nil, "", fmt.Errorf("client config request failed to supply the missing blob: %w", err)
	}
	if string(clientConfig) != string(config) {
		return nil, "", fmt.Errorf("client config request returned unexpected bytes")
	}
	configRequestsAfter, err := countFixturePath(run.context, run.environment, "/v2/registry-hub-test/fixture/blobs/"+configDigest)
	if err != nil {
		return nil, "", err
	}
	if configRequestsAfter != 1 {
		return nil, "", fmt.Errorf("client blob demand fetched the config %d times, want exactly once", configRequestsAfter)
	}
	task, err = waitForSolidifiedTask(run.context, run.environment, "github.com", run.upstreamRepository, "minimal-discovery")
	if err != nil {
		return nil, "", err
	}
	if testkit.String(task["rootDigest"]) != indexDigest || testkit.String(task["mediaType"]) != support.OCIIndexMediaType || testkit.String(task["status"]) != "solidified" || task["rootPublished"] != true {
		return nil, "", fmt.Errorf("compatibility response replaced the original OCI index discovery root")
	}
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
	graphNodes := testkit.Slice(task["graphNodes"])
	if err := verifyDiscoveryGraph(graph, checkpoints, graphNodes, indexDigest, indexBody, childDigest, childManifest, configDigest); err != nil {
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
		"discoveryGraphObjects": len(graphNodes), "discoveryGraphCheckpoints": len(checkpoints),
		"discoveryChildPlatform": platform, "discoveryIndexMediaType": localIndexType,
		"discoveryIndexRawBytesPreserved": string(indexBody) == string(localIndexBody),
		"discoveryChildMediaType":         localChildType, "discoveryChildRawBytesPreserved": string(childManifest) == string(localChildBody),
		"compatibleDockerDigest": discoveryDigest, "compatibleDockerBytesMatch": string(discoveryBody) == dockerListBody,
		"discoveryRequests": requests, "discoveryAccepts": accepts,
		"discoveryBlobPrefetchRequests": configRequestsBefore, "discoveryClientBlobRequests": configRequestsAfter,
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

func verifyDiscoveryGraph(graph, checkpoints map[string]any, graphNodes []any, indexDigest string, indexBody []byte, childDigest string, childManifest []byte, configDigest string) error {
	expected := map[string]struct {
		mediaType string
		size      int
	}{
		indexDigest:  {mediaType: support.OCIIndexMediaType, size: len(indexBody)},
		childDigest:  {mediaType: support.OCIManifestMediaType, size: len(childManifest)},
		configDigest: {mediaType: "application/vnd.oci.image.config.v1+json", size: len([]byte(discoveryConfigBody))},
	}
	if len(graph) != len(expected)-1 || len(checkpoints) != len(expected)-1 || len(graphNodes) != len(expected) {
		return fmt.Errorf("upstream OCI index graph has %d manifest entries, %d persisted nodes, and %d checkpoints, want %d, %d, and %d", len(graph), len(graphNodes), len(checkpoints), len(expected)-1, len(expected), len(expected)-1)
	}
	for digest, object := range expected {
		role := "manifest"
		if object.mediaType == "application/vnd.oci.image.config.v1+json" {
			role = "blob"
		} else {
			if testkit.String(checkpoints[digest]) != "verified" {
				return fmt.Errorf("upstream OCI index manifest %q has no verified checkpoint", digest)
			}
			entry := testkit.Map(graph[digest])
			if testkit.String(entry["digest"]) != digest || testkit.String(entry["mediaType"]) != object.mediaType || testkit.Int(entry["size"]) != int64(object.size) {
				return fmt.Errorf("upstream OCI index manifest entry %q has invalid digest, media type, or size", digest)
			}
		}
		if err := verifyGraphNodeMetadata(graphNodes, digest, role, object.mediaType, object.size); err != nil {
			return err
		}
	}
	return nil
}
