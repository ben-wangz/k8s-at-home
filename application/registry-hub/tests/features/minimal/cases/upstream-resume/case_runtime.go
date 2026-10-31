package upstreamresume

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	upstreamauthtransport "github.com/ben-wangz/registry-hub-tests/features/upstream-auth-transport"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func configureBackendRuntime(ctx context.Context, environment testkit.CaseContext, requestSeconds, idleSeconds int) error {
	if err := waitForProtectedWritesToSettle(ctx, environment); err != nil {
		return fmt.Errorf("wait for protected writes before Backend restart: %w", err)
	}
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

func waitForProtectedWritesToSettle(ctx context.Context, environment testkit.CaseContext) error {
	deadline := time.NewTimer(5 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var settledSince time.Time
	for {
		settled, err := protectedWritesSettled(ctx, environment)
		if err != nil {
			return err
		}
		if settled {
			if settledSince.IsZero() {
				settledSince = time.Now()
			} else if time.Since(settledSince) >= 10*time.Second {
				return nil
			}
		} else {
			settledSince = time.Time{}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("protected writes did not remain settled for ten seconds within five minutes")
		case <-ticker.C:
		}
	}
}

func protectedWritesSettled(ctx context.Context, environment testkit.CaseContext) (bool, error) {
	token := ""
	for {
		query := url.Values{"limit": {"100"}}
		if token != "" {
			query.Set("token", token)
		}
		response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/gc?"+query.Encode(), nil, nil)
		if err != nil {
			return false, err
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			return false, fmt.Errorf("GC protection status returned %s", response.Status)
		}
		var status map[string]any
		if _, err := support.DecodeJSON(response, 1<<20, &status); err != nil {
			return false, err
		}
		gate := testkit.Map(status["gate"])
		if gate["recoveryBarrier"] == true {
			return false, nil
		}
		for _, rawSession := range testkit.Slice(status["sessions"]) {
			session := testkit.Map(rawSession)
			if testkit.Int(session["pendingOperations"]) > 0 || session["uncertainRemoteResult"] == true {
				return false, nil
			}
		}
		token = testkit.String(status["nextToken"])
		if token == "" {
			return true, nil
		}
	}
}
