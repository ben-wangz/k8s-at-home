package upstreammirror

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

//go:embed latency/case.json
var latencyMetadata embed.FS

func NewDiscoveryLatency() (testkit.Case, error) {
	files, err := fs.Sub(latencyMetadata, "latency")
	if err != nil {
		return testkit.Case{}, err
	}
	definition, raw, err := testkit.LoadDefinition(files, "upstream-discovery-latency")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: runDiscoveryLatency}, nil
}

func runDiscoveryLatency(ctx context.Context, environment testkit.CaseContext) (err error) {
	if err := captureLatencyEvidence(ctx, environment, "before"); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, captureLatencyEvidence(ctx, environment, "after")) }()
	values, err := verifyClosureDepthLimit(ctx, environment, support.NewRegistryClient(environment), "github.com/registry-hub-test/fixture", "registry-hub-test/fixture")
	if err != nil {
		return err
	}
	return support.WriteResult(environment, values)
}

func captureLatencyEvidence(ctx context.Context, environment testkit.CaseContext, phase string) error {
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for name, path := range map[string]string{"metrics": "/metrics", "workers": "/api/v1/workers"} {
		response, err := support.AdminRequest(bounded, environment, http.MethodGet, path, nil, nil)
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 8<<20))
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			return errors.Join(readErr, closeErr)
		}
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("latency %s evidence returned %s", name, response.Status)
		}
		if err := os.WriteFile(filepath.Join(environment.OutputDir, phase+"-"+name+".txt"), body, 0600); err != nil {
			return err
		}
	}
	return nil
}
