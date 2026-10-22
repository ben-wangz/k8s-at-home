package upstreammirror

import (
	"context"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func PrepareExternalFixture(ctx context.Context, environment testkit.CaseContext) (*support.RegistryClient, string, error) {
	run, err := setupMirrorRun(ctx, environment)
	if err != nil {
		return nil, "", err
	}
	return run.registry, run.remoteRepository, nil
}
