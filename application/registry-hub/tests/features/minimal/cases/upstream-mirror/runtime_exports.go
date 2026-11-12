package upstreammirror

import (
	"context"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func SetBackendEnvironment(ctx context.Context, environment testkit.CaseContext, entries ...string) error {
	return setBackendEnvironment(ctx, environment, entries...)
}

func WaitForTaskStatus(ctx context.Context, environment testkit.CaseContext, reference, status string, timeout time.Duration) (map[string]any, error) {
	return waitForTaskStatusWithin(ctx, environment, "github.com", "registry-hub-test/fixture", reference, status, timeout)
}

func CountFixturePath(ctx context.Context, environment testkit.CaseContext, path string) (int, error) {
	return countFixturePath(ctx, environment, path)
}
