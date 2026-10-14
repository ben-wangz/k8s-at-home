package upstreammirror

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"mime"
	"net/http"
)

func verifyInlineAndURLDescriptors(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, repository, upstreamRepository string) (map[string]any, error) {
	inlineConfig := []byte("upstream-inline-config")
	inlineLayer := []byte("upstream-url-layer")
	inlineSubject := []byte("upstream-inline-subject")
	inlineURL := "https://github.com/v2/registry-hub-test/fixture/url-must-not-fetch"
	inlineBody, inlineHeaders, err := support.ReadManifest(ctx, registry, repository, "minimal-inline-data", support.OCIManifestMediaType)
	if err != nil {
		return nil, err
	}
	inlineDigest := support.Digest(inlineBody)
	if inlineHeaders.Get("Docker-Content-Digest") != inlineDigest {
		return nil, fmt.Errorf("inline descriptor manifest digest header is invalid")
	}
	inlineTask, err := waitForSolidifiedTask(ctx, environment, "github.com", upstreamRepository, "minimal-inline-data")
	if err != nil {
		return nil, err
	}
	if testkit.String(inlineTask["rootDigest"]) != inlineDigest || testkit.String(inlineTask["status"]) != "solidified" || inlineTask["rootPublished"] != true {
		return nil, fmt.Errorf("inline descriptor manifest root did not solidify")
	}
	var inlineManifest support.Manifest
	if err := json.Unmarshal(inlineBody, &inlineManifest); err != nil || inlineManifest.Config == nil || len(inlineManifest.Layers) != 1 {
		return nil, fmt.Errorf("inline descriptor fixture manifest is invalid")
	}
	inlineSubjectDigest := support.Digest(inlineSubject)
	inlineConfigDigest := support.Digest(inlineConfig)
	inlineLayerDigest := support.Digest(inlineLayer)
	if inlineManifest.Config.Digest != inlineConfigDigest || inlineManifest.Config.Size != len(inlineConfig) || inlineManifest.Config.Data != base64.StdEncoding.EncodeToString(inlineConfig) {
		return nil, fmt.Errorf("upstream descriptor.data identity or encoding changed")
	}
	if inlineManifest.Layers[0].Digest != inlineLayerDigest || inlineManifest.Layers[0].Size != len(inlineLayer) || len(inlineManifest.Layers[0].URLs) != 1 || inlineManifest.Layers[0].URLs[0] != inlineURL {
		return nil, fmt.Errorf("upstream descriptor.urls identity or value changed")
	}
	if inlineManifest.ArtifactType != "application/vnd.registry-hub.test.inline" || inlineManifest.Annotations["org.opencontainers.image.title"] != "inline fixture" || inlineManifest.Subject == nil || inlineManifest.Subject.Digest != inlineSubjectDigest || inlineManifest.Subject.Size != len(inlineSubject) || inlineManifest.Subject.Annotations["org.opencontainers.image.title"] != "unfetched subject" || inlineManifest.Config.Annotations["org.opencontainers.image.title"] != "inline config" || inlineManifest.Layers[0].ArtifactType != "application/vnd.registry-hub.test.layer" || inlineManifest.Layers[0].Annotations["org.opencontainers.image.title"] != "url-bearing layer" {
		return nil, fmt.Errorf("upstream manifest standard metadata changed")
	}
	var inlineRaw map[string]any
	if err := json.Unmarshal(inlineBody, &inlineRaw); err != nil {
		return nil, err
	}
	configRaw := testkit.Map(inlineRaw["config"])
	layersRaw := testkit.Slice(inlineRaw["layers"])
	if testkit.Map(inlineRaw["x-fixture-manifest"])["retained"] != true || testkit.String(configRaw["x-fixture-descriptor"]) != "config-retained" || len(layersRaw) != 1 || testkit.String(testkit.Map(layersRaw[0])["x-fixture-descriptor"]) != "layer-retained" || testkit.String(testkit.Map(inlineRaw["subject"])["x-fixture-descriptor"]) != "subject-retained" {
		return nil, fmt.Errorf("upstream manifest extension fields are missing")
	}
	graph := testkit.Map(inlineTask["graph"])
	checkpoints := testkit.Map(inlineTask["checkpoints"])
	if len(graph) != 3 || len(checkpoints) != 3 {
		return nil, fmt.Errorf("inline descriptor graph has %d nodes and %d checkpoints, want 3 each", len(graph), len(checkpoints))
	}
	for _, digest := range []string{inlineDigest, inlineConfigDigest, inlineLayerDigest} {
		if testkit.String(checkpoints[digest]) != "verified" {
			return nil, fmt.Errorf("inline descriptor graph object %q has no verified checkpoint", digest)
		}
	}
	localManifest, localHeaders, err := support.ReadManifest(ctx, registry, repository, "minimal-inline-data", support.OCIManifestMediaType)
	if err != nil || string(localManifest) != string(inlineBody) || localHeaders.Get("Docker-Content-Digest") != inlineDigest {
		return nil, fmt.Errorf("solidified descriptor manifest bytes or digest changed")
	}
	inlineMediaType, _, err := mime.ParseMediaType(localHeaders.Get("Content-Type"))
	if err != nil || inlineMediaType != support.OCIManifestMediaType {
		return nil, fmt.Errorf("solidified descriptor manifest media type changed")
	}
	configBlob, err := support.ReadBlob(ctx, registry, repository, inlineConfigDigest)
	if err != nil || string(configBlob) != string(inlineConfig) {
		return nil, fmt.Errorf("embedded descriptor.data blob was not imported with its bytes")
	}
	layerBlob, err := support.ReadBlob(ctx, registry, repository, inlineLayerDigest)
	if err != nil || string(layerBlob) != string(inlineLayer) {
		return nil, fmt.Errorf("URL-bearing descriptor standard blob was not imported")
	}
	inlineConfigRequests, err := countFixturePath(ctx, environment, "/v2/registry-hub-test/fixture/blobs/"+inlineConfigDigest)
	if err != nil {
		return nil, err
	}
	inlineLayerRequests, err := countFixturePath(ctx, environment, "/v2/registry-hub-test/fixture/blobs/"+inlineLayerDigest)
	if err != nil {
		return nil, err
	}
	inlineURLRequests, err := countFixturePath(ctx, environment, "/v2/registry-hub-test/fixture/url-must-not-fetch")
	if err != nil {
		return nil, err
	}
	if inlineConfigRequests != 0 || inlineLayerRequests != 1 || inlineURLRequests != 0 {
		return nil, fmt.Errorf("descriptor import upstream requests: inline blob=%d standard blob=%d URL=%d", inlineConfigRequests, inlineLayerRequests, inlineURLRequests)
	}
	inlineSubjectRequests, err := countFixturePath(ctx, environment, "/v2/registry-hub-test/fixture/manifests/"+inlineSubjectDigest)
	if err != nil {
		return nil, err
	}
	if inlineSubjectRequests != 0 {
		return nil, fmt.Errorf("manifest subject was fetched %d times", inlineSubjectRequests)
	}
	missingLayer := []byte("upstream-missing-layer")
	missingLayerDigest := support.Digest(missingLayer)
	missingURL := "https://github.com/v2/registry-hub-test/fixture/url-must-not-fetch-missing"
	missingBody, _, err := support.ReadManifest(ctx, registry, repository, "minimal-url-missing", support.OCIManifestMediaType)
	if err != nil {
		return nil, err
	}
	var missingManifest support.Manifest
	if err := json.Unmarshal(missingBody, &missingManifest); err != nil || len(missingManifest.Layers) != 1 || len(missingManifest.Layers[0].URLs) != 1 || missingManifest.Layers[0].URLs[0] != missingURL {
		return nil, fmt.Errorf("missing standard blob fixture has invalid descriptor.urls")
	}
	missingTask, err := waitForFailedTask(ctx, environment, "github.com", upstreamRepository, "minimal-url-missing")
	if err != nil {
		return nil, err
	}
	if testkit.String(missingTask["status"]) != "failed" || testkit.String(missingTask["rootDigest"]) == "" || missingTask["rootPublished"] == true {
		return nil, fmt.Errorf("missing standard blob task state is invalid: status=%q root=%q published=%v", testkit.String(missingTask["status"]), testkit.String(missingTask["rootDigest"]), missingTask["rootPublished"])
	}
	missingBlobRequests, err := countFixturePath(ctx, environment, "/v2/registry-hub-test/fixture/blobs/"+missingLayerDigest)
	if err != nil {
		return nil, err
	}
	missingURLRequests, err := countFixturePath(ctx, environment, "/v2/registry-hub-test/fixture/url-must-not-fetch-missing")
	if err != nil {
		return nil, err
	}
	if missingBlobRequests != 1 || missingURLRequests != 0 {
		return nil, fmt.Errorf("missing descriptor requests: standard blob=%d URL=%d", missingBlobRequests, missingURLRequests)
	}
	return map[string]any{
		"inlineDataRootDigest":           inlineDigest,
		"inlineDataRootPublished":        inlineTask["rootPublished"],
		"inlineManifestFieldsPreserved":  true,
		"inlineManifestMediaType":        inlineMediaType,
		"inlineSubjectDigest":            inlineSubjectDigest,
		"inlineSubjectNotFetched":        inlineSubjectRequests == 0 && graph[inlineSubjectDigest] == nil,
		"inlineSubjectPresentInGraph":    graph[inlineSubjectDigest] != nil,
		"inlineConfigBlobDigest":         inlineConfigDigest,
		"inlineLayerBlobDigest":          inlineLayerDigest,
		"inlineGraphObjects":             len(graph),
		"inlineGraphCheckpoints":         len(checkpoints),
		"inlineManifestBytesPreserved":   string(inlineBody) == string(localManifest),
		"inlineConfigBytesMatch":         string(configBlob) == string(inlineConfig),
		"inlineLayerBytesMatch":          string(layerBlob) == string(inlineLayer),
		"inlineConfigUpstreamRequests":   inlineConfigRequests,
		"inlineLayerUpstreamRequests":    inlineLayerRequests,
		"inlineURLRequests":              inlineURLRequests,
		"inlineSubjectRequests":          inlineSubjectRequests,
		"missingURLTaskStatus":           testkit.String(missingTask["status"]),
		"missingURLDiscoveredRootDigest": testkit.String(missingTask["rootDigest"]),
		"missingURLRootPublished":        missingTask["rootPublished"],
		"missingURLStandardBlobRequests": missingBlobRequests,
		"missingURLRequests":             missingURLRequests,
	}, nil
}

func verifyInvalidInlineData(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, repository, upstreamRepository string) (map[string]any, error) {
	tests := []struct {
		reference string
		prefix    string
	}{
		{reference: "minimal-invalid-inline-data", prefix: "invalidInlineData"},
		{reference: "minimal-invalid-inline-size", prefix: "invalidInlineSize"},
		{reference: "minimal-invalid-inline-digest", prefix: "invalidInlineDigest"},
		{reference: "minimal-invalid-inline-type", prefix: "invalidInlineType"},
	}
	result := make(map[string]any, len(tests)*6)
	for _, test := range tests {
		status, err := upstreamManifestStatus(ctx, registry, repository, test.reference)
		if err != nil {
			return nil, err
		}
		if status != http.StatusBadGateway {
			return nil, fmt.Errorf("invalid upstream descriptor.data %q returned %d, want 502", test.reference, status)
		}
		task, err := waitForFailedTask(ctx, environment, "github.com", upstreamRepository, test.reference)
		if err != nil {
			return nil, err
		}
		graph := testkit.Map(task["graph"])
		checkpoints := testkit.Map(task["checkpoints"])
		if testkit.String(task["errorClass"]) != "manifest_invalid" || testkit.String(task["rootDigest"]) != "" || task["rootPublished"] == true || task["retryable"] == true || len(graph) != 0 || len(checkpoints) != 0 {
			return nil, fmt.Errorf("invalid descriptor.data %q left root or graph state: class=%q root=%q published=%v graph=%d checkpoints=%d", test.reference, testkit.String(task["errorClass"]), testkit.String(task["rootDigest"]), task["rootPublished"], len(graph), len(checkpoints))
		}
		requests, err := countFixtureRequests(ctx, environment, test.reference)
		if err != nil {
			return nil, err
		}
		if requests != 1 {
			return nil, fmt.Errorf("invalid inline-data manifest %q requests = %d, want 1", test.reference, requests)
		}
		result[test.prefix+"Status"] = status
		result[test.prefix+"TaskStatus"] = testkit.String(task["status"])
		result[test.prefix+"ErrorClass"] = testkit.String(task["errorClass"])
		result[test.prefix+"RootUnpublished"] = task["rootPublished"] != true && testkit.String(task["rootDigest"]) == ""
		result[test.prefix+"GraphObjects"] = len(graph)
		result[test.prefix+"Checkpoints"] = len(checkpoints)
		result[test.prefix+"Requests"] = requests
	}
	return result, nil
}
