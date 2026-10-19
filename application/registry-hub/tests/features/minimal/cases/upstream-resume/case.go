package upstreamresume

import (
	"context"
	"embed"
	"fmt"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

const upstreamRepository = "registry-hub-test/fixture"

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "upstream-resume")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) (runErr error) {
	registry := support.NewRegistryClient(environment)
	results := make(map[string]any)
	settingsChanged := false
	defer func() {
		if settingsChanged {
			restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Minute)
			defer cancel()
			if err := configureBackendRuntime(restoreCtx, environment, 300, 60); runErr == nil && err != nil {
				runErr = fmt.Errorf("restore Backend download settings: %w", err)
			}
		}
		if runErr == nil {
			runErr = support.WriteResult(environment, results)
		}
	}()

	for _, scenario := range []struct {
		mode string
		want []int64
	}{
		{mode: "healthy"},
		{mode: "basic", want: []int64{1 << 20}},
		{mode: "multi", want: []int64{1 << 20, 5 << 20 / 4, 3 << 19, 7 << 18}},
		{mode: "no-range", want: []int64{1 << 20}},
		{mode: "etag-change", want: []int64{1 << 20}},
		{mode: "range-416", want: []int64{1 << 20}},
	} {
		evidence, err := verifyResumeBlob(ctx, environment, registry, scenario.mode, scenario.want)
		if err != nil {
			return fmt.Errorf("verify %s Blob transfer: %w", scenario.mode, err)
		}
		results["mode_"+scenario.mode] = evidence
	}

	settingsChanged = true
	if err := configureBackendRuntime(ctx, environment, 1, 10); err != nil {
		return err
	}
	if results["mode_timeout"], runErr = verifyResumeBlob(ctx, environment, registry, "timeout", []int64{1 << 20, 5 << 20 / 4}); runErr != nil {
		return fmt.Errorf("verify short Range attempt timeout: %w", runErr)
	}
	if err := configureBackendRuntime(ctx, environment, 5, 1); err != nil {
		return err
	}
	if results["mode_idle"], runErr = verifyResumeBlob(ctx, environment, registry, "idle", []int64{1 << 20}); runErr != nil {
		return fmt.Errorf("verify upstream idle timeout: %w", runErr)
	}
	if err := configureBackendRuntime(ctx, environment, 1, 3); err != nil {
		return err
	}
	if results["mode_slow"], runErr = verifySlowFullResponse(ctx, environment, registry); runErr != nil {
		return fmt.Errorf("verify slow full 200 response: %w", runErr)
	}
	if err := configureBackendRuntime(ctx, environment, 300, 60); err != nil {
		return err
	}
	if results["concurrent"], runErr = verifyConcurrentReaders(ctx, environment, registry); runErr != nil {
		return fmt.Errorf("verify concurrent readers: %w", runErr)
	}
	if results["restart"], runErr = verifyBackendRestart(ctx, environment, registry); runErr != nil {
		return fmt.Errorf("verify Backend restart resume: %w", runErr)
	}
	if results["background"], runErr = verifyBackgroundRetry(ctx, environment, registry); runErr != nil {
		return fmt.Errorf("verify background retry resume: %w", runErr)
	}
	if results["podman"], runErr = verifyPodmanResume(ctx, environment); runErr != nil {
		return fmt.Errorf("verify Podman pull: %w", runErr)
	}
	return nil
}
