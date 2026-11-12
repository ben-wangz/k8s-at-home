package gc

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
)

func waitForRegistryService(ctx context.Context, registry *support.RegistryClient) error {
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var lastErr error
	for {
		attemptCtx, attemptCancel := context.WithTimeout(readyCtx, 2*time.Second)
		response, err := registry.Version(attemptCtx)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				attemptCancel()
				return nil
			}
			if response.StatusCode != http.StatusServiceUnavailable {
				attemptCancel()
				return fmt.Errorf("registry Service readiness returned %s", response.Status)
			}
			lastErr = fmt.Errorf("registry Service returned %s", response.Status)
		} else {
			lastErr = err
		}
		attemptCancel()
		select {
		case <-readyCtx.Done():
			return fmt.Errorf("registry Service did not become ready: %w; last attempt: %v", readyCtx.Err(), lastErr)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
