package gc

import (
	"context"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"strings"
)

func fixtureBlobRequestCount(ctx context.Context, environment testkit.CaseContext, digest string) (int, error) {
	evidence, err := fixtureBlobRequestEvidenceFor(ctx, environment, digest)
	if err != nil {
		return 0, err
	}
	return evidence[digest].AnonymousRequestCount, nil
}

type fixtureBlobRequestEvidence struct {
	RequestCount          int      `json:"requestCount"`
	AnonymousRequestCount int      `json:"anonymousRequestCount"`
	RequestLines          []string `json:"requestLines,omitempty"`
}

func fixtureBlobRequestEvidenceFor(ctx context.Context, environment testkit.CaseContext, digests ...string) (map[string]fixtureBlobRequestEvidence, error) {
	result, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", "pod/"+environment.Release+"-external-upstream-proxy", "-c", "proxy", "--tail=2000")
	if err != nil {
		return nil, fmt.Errorf("read upstream fixture request evidence: %w", err)
	}
	evidence := make(map[string]fixtureBlobRequestEvidence, len(digests))
	for _, digest := range digests {
		evidence[digest] = fixtureBlobRequestEvidence{}
	}
	for _, line := range strings.Split(string(result.Stdout), "\n") {
		for _, digest := range digests {
			marker := "host=github.com path=/v2/registry-hub-test/fixture/blobs/" + digest + " "
			if !strings.Contains(line, marker) {
				continue
			}
			item := evidence[digest]
			item.RequestCount++
			if strings.Contains(line, " auth=absent ") {
				item.AnonymousRequestCount++
			}
			if len(item.RequestLines) < 5 {
				item.RequestLines = append(item.RequestLines, line)
			}
			evidence[digest] = item
		}
	}
	return evidence, nil
}
