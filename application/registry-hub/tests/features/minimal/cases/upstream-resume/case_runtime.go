package upstreamresume

import (
	"context"
	"fmt"

	upstreamauthtransport "github.com/ben-wangz/registry-hub-tests/features/upstream-auth-transport"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func configureBackendRuntime(ctx context.Context, environment testkit.CaseContext, requestSeconds, idleSeconds int) error {
	_, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "set", "env", "deployment/"+environment.Fullname, "--overwrite=true",
		fmt.Sprintf("REGISTRY_HUB_REQUEST_TIMEOUT_SECONDS=%d", requestSeconds),
		fmt.Sprintf("REGISTRY_HUB_UPSTREAM_BODY_IDLE_TIMEOUT_SECONDS=%d", idleSeconds))
	if err != nil {
		return fmt.Errorf("set Backend download timeouts: %w", err)
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "status", "deployment/"+environment.Fullname, "--timeout=180s"); err != nil {
		return fmt.Errorf("wait for Backend timeout update: %w", err)
	}
	if err := upstreamauthtransport.WaitBackendServiceEndpoint(ctx, environment); err != nil {
		return err
	}
	return nil
}
