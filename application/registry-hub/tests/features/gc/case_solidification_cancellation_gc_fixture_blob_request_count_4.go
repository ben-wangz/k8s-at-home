package gc

import (
	"context"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"strings"
)

func fixtureBlobRequestCount(ctx context.Context, environment testkit.CaseContext, digest string) (int, error) {
	result, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", "pod/"+environment.Release+"-external-upstream-proxy", "-c", "proxy", "--tail=2000")
	if err != nil {
		return 0, fmt.Errorf("read upstream fixture request evidence: %w", err)
	}
	marker := "host=github.com path=/v2/registry-hub-test/fixture/blobs/" + digest + " auth=absent"
	count := 0
	for _, line := range strings.Split(string(result.Stdout), "\n") {
		if strings.Contains(line, marker) {
			count++
		}
	}
	return count, nil
}
