package upstreammirror

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed large/case.json
var largeMetadata embed.FS

func NewLargeLayerPerformance() (testkit.Case, error) {
	files, err := fs.Sub(largeMetadata, "large")
	if err != nil {
		return testkit.Case{}, err
	}
	definition, raw, err := testkit.LoadDefinition(files, "large-layer-performance")
	return testkit.Case{Definition: definition, Raw: raw, Run: runLargeLayerPerformance}, err
}

func runLargeLayerPerformance(ctx context.Context, environment testkit.CaseContext) (runErr error) {
	run, err := setupMirrorRun(ctx, environment)
	if err != nil {
		return err
	}
	run.registry.Client.Timeout = 20 * time.Minute
	defer func() {
		restore, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Minute)
		defer cancel()
		runErr = errors.Join(runErr, setBackendEnvironment(restore, environment,
			"REGISTRY_HUB_SOLIDIFICATION_MAX_PROTECTION_SESSIONS-", "REGISTRY_HUB_SOLIDIFICATION_MAX_UNANCHORED_DISTRIBUTION_BYTES-", "REGISTRY_HUB_WORKERS=4"))
	}()
	results := make([]map[string]any, 0, 3)
	for _, mib := range []int{64, 256, 1024} {
		if err := testkit.RunPhase(environment, fmt.Sprintf("layer-%d-mib", mib), func() error {
			result, err := measureLargeLayer(run, mib)
			if err == nil {
				results = append(results, result)
				err = testkit.WriteJSONAtomic(filepath.Join(environment.OutputDir, "measurements.json"), results)
			}
			return err
		}); err != nil {
			return err
		}
	}
	return support.WriteResult(environment, map[string]any{"sizes": results, "ordinarySlots": 2, "taskWorkers": 4, "sizeOrderMiB": []int{64, 256, 1024}, "cacheTemperature": "uncontrolled_os_page_cache", "intentionalHoldExcludedFromReleaseToAnchor": true})
}

func largeEvidence(ctx context.Context, environment testkit.CaseContext, phase string) error {
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := captureLatencyEvidence(bounded, environment, phase); err != nil {
		return err
	}
	logs, err := testkit.Command(bounded, "kubectl", "-n", environment.Namespace, "logs", "deployment/"+environment.Fullname, "-c", "backend", "--tail=20000")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(environment.OutputDir, phase+"-backend.log"), logs.Stdout, 0600)
}
