package upstreammirror

import (
	"context"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"mime"
	"net/http"
)

func verifyNestedPlatformClosure(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, repository, upstreamRepository string) (map[string]any, error) {
	root, objects, config, err := expectedNestedDiscoveryFixture()
	if err != nil {
		return nil, err
	}
	rootBody, rootHeaders, err := support.ReadManifest(ctx, registry, repository, "minimal-nested-discovery", support.OCIIndexMediaType)
	if err != nil {
		return nil, err
	}
	if string(rootBody) != string(root.body) || rootHeaders.Get("Docker-Content-Digest") != support.Digest(root.body) {
		return nil, fmt.Errorf("nested discovery root bytes or digest differ from the fixture")
	}
	rootDigest := support.Digest(root.body)
	configDigest := support.Digest(config)
	expected := make(map[string]nestedExpectedObject, len(objects)+2)
	for digest, object := range objects {
		expected[digest] = object
	}
	expected[rootDigest] = root
	expected[configDigest] = nestedExpectedObject{body: config, mediaType: "application/vnd.oci.image.config.v1+json"}
	task, err := waitForTaskStatus(ctx, environment, "github.com", upstreamRepository, "minimal-nested-discovery", "waiting_dependencies")
	if err != nil {
		return nil, fmt.Errorf("nested manifest discovery did not wait for client blob demand: %w", err)
	}
	configRequestsBefore, err := countFixturePath(ctx, environment, "/v2/registry-hub-test/fixture/blobs/"+configDigest)
	if err != nil {
		return nil, err
	}
	if configRequestsBefore != 0 {
		return nil, fmt.Errorf("nested manifest discovery fetched an unrequested config blob %d times", configRequestsBefore)
	}
	clientConfig, err := support.ReadBlob(ctx, registry, repository, configDigest)
	if err != nil {
		return nil, fmt.Errorf("client config request failed to supply the missing nested blob: %w", err)
	}
	if string(clientConfig) != string(config) {
		return nil, fmt.Errorf("client config request returned unexpected bytes")
	}
	task, err = waitForSolidifiedTask(ctx, environment, "github.com", upstreamRepository, "minimal-nested-discovery")
	if err != nil {
		return nil, err
	}
	if testkit.String(task["rootDigest"]) != rootDigest || testkit.String(task["mediaType"]) != support.OCIIndexMediaType || task["rootPublished"] != true {
		return nil, fmt.Errorf("nested OCI index did not solidify as its discovery root")
	}
	graph := testkit.Map(task["graph"])
	checkpoints := testkit.Map(task["checkpoints"])
	graphNodes := testkit.Slice(task["graphNodes"])
	if len(graph) != len(expected)-1 || len(checkpoints) != len(expected)-1 || len(graphNodes) != len(expected) {
		return nil, fmt.Errorf("nested OCI graph has %d manifest entries, %d persisted nodes, and %d checkpoints, want %d, %d, and %d", len(graph), len(graphNodes), len(checkpoints), len(expected)-1, len(expected), len(expected)-1)
	}
	platforms := make([]string, 0, 4)
	for digest, object := range expected {
		role := "manifest"
		if object.mediaType == "application/vnd.oci.image.config.v1+json" {
			role = "blob"
		} else {
			if testkit.String(checkpoints[digest]) != "verified" {
				return nil, fmt.Errorf("nested OCI manifest %q has no verified checkpoint", digest)
			}
			entry := testkit.Map(graph[digest])
			if testkit.String(entry["digest"]) != digest || testkit.String(entry["mediaType"]) != object.mediaType || testkit.Int(entry["size"]) != int64(len(object.body)) {
				return nil, fmt.Errorf("nested OCI manifest %q has invalid digest, media type, or size", digest)
			}
		}
		if err := verifyGraphNodeMetadata(graphNodes, digest, role, object.mediaType, len(object.body)); err != nil {
			return nil, err
		}
		entry := testkit.Map(graph[digest])
		if object.platform != nil {
			platform := testkit.Map(entry["platform"])
			for name, value := range object.platform {
				if testkit.String(platform[name]) != value {
					return nil, fmt.Errorf("nested OCI platform %q lost %s=%q", digest, name, value)
				}
			}
			platforms = append(platforms, object.platform["os"]+"/"+object.platform["architecture"])
		}
		var body []byte
		var headers http.Header
		if object.mediaType == "application/vnd.oci.image.config.v1+json" {
			body, err = support.ReadBlob(ctx, registry, repository, digest)
		} else {
			body, headers, err = support.ReadManifest(ctx, registry, repository, digest, object.mediaType)
		}
		if err != nil || string(body) != string(object.body) {
			return nil, fmt.Errorf("nested OCI object %q is not locally readable with its original bytes", digest)
		}
		if headers != nil {
			mediaType, _, parseErr := mime.ParseMediaType(headers.Get("Content-Type"))
			if parseErr != nil || mediaType != object.mediaType || headers.Get("Docker-Content-Digest") != digest {
				return nil, fmt.Errorf("nested OCI manifest %q lost its media type or digest", digest)
			}
		}
	}
	rootRequests, err := countFixtureRequests(ctx, environment, "minimal-nested-discovery")
	if err != nil {
		return nil, err
	}
	configRequests, err := countFixturePath(ctx, environment, "/v2/registry-hub-test/fixture/blobs/"+configDigest)
	if err != nil {
		return nil, err
	}
	nestedURLRequests, err := countFixturePath(ctx, environment, "/v2/registry-hub-test/fixture/url-must-not-fetch")
	if err != nil {
		return nil, err
	}
	if rootRequests != 1 || configRequests < 1 || nestedURLRequests != 0 || len(platforms) != 4 {
		return nil, fmt.Errorf("nested OCI request/platform counts are root=%d config=%d urls=%d platforms=%d", rootRequests, configRequests, nestedURLRequests, len(platforms))
	}
	return map[string]any{
		"nestedRootDigest":             rootDigest,
		"nestedGraphObjects":           len(graphNodes),
		"nestedGraphCheckpoints":       len(checkpoints),
		"nestedPlatforms":              platforms,
		"nestedConfigDigest":           configDigest,
		"nestedSharedConfigRequests":   configRequests,
		"nestedDescriptorURLRequests":  nestedURLRequests,
		"nestedRootRequests":           rootRequests,
		"nestedLocalRawBytesPreserved": true,
		"nestedRootPublished":          task["rootPublished"],
	}, nil
}
