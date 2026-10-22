package upstreammirror

import (
	"context"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
)

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
		graphNodes := testkit.Slice(task["graphNodes"])
		if testkit.String(task["errorClass"]) != "manifest_invalid" || testkit.String(task["rootDigest"]) != "" || task["rootPublished"] == true || task["retryable"] == true || len(graph) != 0 || len(graphNodes) != 0 || len(checkpoints) != 0 {
			return nil, fmt.Errorf("invalid descriptor.data %q left root or graph state: class=%q root=%q published=%v graph=%d persisted=%d checkpoints=%d", test.reference, testkit.String(task["errorClass"]), testkit.String(task["rootDigest"]), task["rootPublished"], len(graph), len(graphNodes), len(checkpoints))
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
