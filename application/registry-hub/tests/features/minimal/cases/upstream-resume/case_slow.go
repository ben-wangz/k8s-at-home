package upstreamresume

import (
	"context"
	"fmt"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func verifySlowFullResponse(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient) (map[string]any, error) {
	evidence, err := verifyResumeBlob(ctx, environment, registry, "slow", nil)
	if err != nil {
		return nil, err
	}
	elapsed := testkit.Int(evidence["elapsedMillis"])
	status200, ok := evidence["status200"].(int)
	if elapsed < 1900 || !ok || status200 != 1 {
		return nil, fmt.Errorf("slow full response evidence is invalid: elapsed=%dms status200=%v", elapsed, evidence["status200"])
	}
	return map[string]any{
		"reference": evidence["reference"], "digest": evidence["digest"], "size": evidence["size"],
		"rangeOffsets": evidence["rangeOffsets"], "elapsedMillis": elapsed,
		"requestTimeoutSeconds": 1, "idleTimeoutSeconds": 3,
	}, nil
}
