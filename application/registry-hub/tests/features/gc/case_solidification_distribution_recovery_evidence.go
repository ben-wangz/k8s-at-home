package gc

import (
	"context"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func directDistributionRecoveryFailureEvidence(ctx context.Context, environment testkit.CaseContext, repository, reference string, since time.Time, priorRoot map[string]any, manifest support.Manifest, podmanPull, blobTask map[string]any, failure error) map[string]any {
	evidence := rootCompletionFailureDiagnostics(ctx, environment, repository, reference, since,
		testkit.String(priorRoot["id"]), testkit.String(priorRoot["roundID"]), manifest, podmanPull, blobTask, failure)
	if status, ok := evidence["gcStatus"].(map[string]any); ok {
		evidence["protectionOwnership"] = protectionCleanupFailureEvidence(ctx, environment, status,
			map[string]any{"taskIDs": []string{testkit.String(blobTask["id"])}}, failure)
	}
	return evidence
}
