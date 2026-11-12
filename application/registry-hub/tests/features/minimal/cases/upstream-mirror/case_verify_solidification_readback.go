package upstreammirror

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func backendLogLines(ctx context.Context, environment testkit.CaseContext, since time.Time) ([]string, error) {
	result, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", "deployment/"+environment.Fullname, "-c", "backend", "--since-time="+since.UTC().Format(time.RFC3339Nano), "--tail=20000")
	if err != nil {
		return nil, fmt.Errorf("read backend solidification logs: %w", err)
	}
	return strings.Split(strings.TrimSpace(string(result.Stdout)), "\n"), nil
}

func verifySolidificationReadback(lines []string, digest string) (map[string]any, error) {
	found := false
	for _, line := range lines {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if event["msg"] != "solidification blob verified" || event["blob_digest"] != digest {
			continue
		}
		found = true
		readbackBytes, ok := event["distribution_readback_bytes"].(float64)
		if !ok || readbackBytes != 0 {
			return nil, fmt.Errorf("solidification event for %s reports distribution readback bytes %v", digest, event["distribution_readback_bytes"])
		}
	}
	if !found {
		return nil, fmt.Errorf("backend logs contain no verified solidification event for %s", digest)
	}
	return map[string]any{"solidificationDistributionReadbackBytes": 0}, nil
}
