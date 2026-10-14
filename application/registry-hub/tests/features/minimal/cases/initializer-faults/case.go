package initializerfaults

import (
	"context"
	"embed"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/apimock"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed case.json
var metadata embed.FS

type identity struct {
	Instance       string
	InternalSecret string
	InternalRecord string
	AdminSecret    string
	AdminRecord    string
}

type harness struct {
	environment testkit.CaseContext
	identity    identity
	api         *apimock.Server
	apiDir      string
}

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "initializer-faults")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	if _, err := testkit.Command(ctx, "podman", "pull", environment.ImageReference); err != nil {
		return fmt.Errorf("pull the exact backend test image: %w", err)
	}
	var results []map[string]any
	run := func(name, barrier string, scenario func(*harness) (map[string]any, error)) error {
		result, err := runScenario(ctx, environment, name, barrier, scenario)
		if err != nil {
			return fmt.Errorf("initializer scenario %s: %w", name, err)
		}
		results = append(results, result)
		return nil
	}
	identity := testIdentity(environment)
	if err := run("lost-create-response-and-conflict", "", func(h *harness) (map[string]any, error) {
		h.api.SetFaults(apimock.Faults{DropRecordCreateResponse: true, DropSecretCreateResponse: true, ConflictConfigMapUpdates: 1})
		if err := h.runOne(ctx); err != nil {
			return nil, err
		}
		if err := verifyReadyState(h); err != nil {
			return nil, err
		}
		stats := h.api.Stats()
		if stats.CommittedSecrets != 2 || stats.CreateSecrets != 2 || stats.CreateConfigMaps != 2 || stats.UpdateConfigMaps < 3 {
			return nil, fmt.Errorf("response-loss recovery or conflict retry produced unexpected API operation counts")
		}
		return resultSummary(h, stats), nil
	}); err != nil {
		return err
	}
	if err := run("concurrent-first-initialization", identity.InternalRecord, func(h *harness) (map[string]any, error) {
		if err := h.runConcurrent(ctx); err != nil {
			return nil, err
		}
		if err := verifyReadyState(h); err != nil {
			return nil, err
		}
		stats := h.api.Stats()
		if stats.CommittedSecrets != 2 || stats.BarrierRequests < 2 {
			return nil, fmt.Errorf("concurrent initialization did not reach the barrier or persisted %d Secrets", stats.CommittedSecrets)
		}
		return resultSummary(h, stats), nil
	}); err != nil {
		return err
	}
	if err := run("record-update-failure-and-retry", "", func(h *harness) (map[string]any, error) {
		h.api.SetFaults(apimock.Faults{FailConfigMapUpdateStatus: 500})
		if err := h.expectFailure(ctx, "HTTP 500"); err != nil {
			return nil, err
		}
		before, _ := h.api.Object("secrets", identity.InternalSecret)
		if before == nil || recordStatus(h.api, identity.InternalRecord) != "pending" {
			return nil, fmt.Errorf("failed commit did not preserve the created Secret and pending record")
		}
		h.api.SetFaults(apimock.Faults{})
		if err := h.runOne(ctx); err != nil {
			return nil, err
		}
		after, _ := h.api.Object("secrets", identity.InternalSecret)
		if !sameSecret(before, after) {
			return nil, fmt.Errorf("retry changed a Secret committed before record failure")
		}
		if err := verifyReadyState(h); err != nil {
			return nil, err
		}
		return resultSummary(h, h.api.Stats()), nil
	}); err != nil {
		return err
	}
	if err := run("forbidden-secret-create", "", func(h *harness) (map[string]any, error) {
		h.api.SetFaults(apimock.Faults{FailSecretCreateStatus: 403})
		if err := h.expectFailure(ctx, "HTTP 403"); err != nil {
			return nil, err
		}
		if h.api.Stats().CommittedSecrets != 0 {
			return nil, fmt.Errorf("forbidden Secret create was treated as a successful first initialization")
		}
		return resultSummary(h, h.api.Stats()), nil
	}); err != nil {
		return err
	}
	if err := run("forbidden-record-read", "", func(h *harness) (map[string]any, error) {
		h.api.SetFaults(apimock.Faults{FailConfigMapGetStatus: 403})
		if err := h.expectFailure(ctx, "HTTP 403"); err != nil {
			return nil, err
		}
		if h.api.Stats().CommittedSecrets != 0 || h.api.Objects() != 0 {
			return nil, fmt.Errorf("API permission error caused initialization writes")
		}
		return resultSummary(h, h.api.Stats()), nil
	}); err != nil {
		return err
	}
	if err := run("api-network-reset", "", func(h *harness) (map[string]any, error) {
		h.api.SetFaults(apimock.Faults{DropConfigMapGet: true})
		if err := h.expectFailure(ctx, "Kubernetes API request failed"); err != nil {
			return nil, err
		}
		if h.api.Objects() != 0 {
			return nil, fmt.Errorf("API connection failure caused initialization writes")
		}
		return resultSummary(h, h.api.Stats()), nil
	}); err != nil {
		return err
	}
	if err := run("api-request-timeout", "", func(h *harness) (map[string]any, error) {
		h.api.SetFaults(apimock.Faults{DelayConfigMapGet: 11 * time.Second})
		if err := h.expectFailure(ctx, "timeout"); err != nil {
			return nil, err
		}
		if h.api.Objects() != 0 {
			return nil, fmt.Errorf("API timeout caused initialization writes")
		}
		return resultSummary(h, h.api.Stats()), nil
	}); err != nil {
		return err
	}
	for _, scenario := range []string{"missing-ready-secret", "corrupt-ready-secret", "invalid-tls-certificate", "wrong-tls-san", "expired-tls-certificate", "changed-secret-uid", "missing-record-recovery", "incompatible-record-scope"} {
		if err := run(scenario, "", func(h *harness) (map[string]any, error) {
			return verifyPersistedStateScenario(ctx, h, scenario)
		}); err != nil {
			return err
		}
	}
	return support.WriteResult(environment, map[string]any{"scenarios": results, "productionImage": environment.ImageReference})
}

func runScenario(ctx context.Context, environment testkit.CaseContext, name, barrier string, scenario func(*harness) (map[string]any, error)) (map[string]any, error) {
	apiDir := filepath.Join(environment.OutputDir, "api-mock-"+name)
	if err := os.RemoveAll(apiDir); err != nil {
		return nil, err
	}
	defer os.RemoveAll(apiDir)
	api, err := apimock.New(apiDir, barrier)
	if err != nil {
		return nil, err
	}
	defer api.Close()
	h := &harness{environment: environment, identity: testIdentity(environment), api: api, apiDir: apiDir}
	return scenario(h)
}

func (h *harness) runOne(ctx context.Context) error {
	runContext, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	arguments := h.podmanArguments()
	_, err := testkit.Command(runContext, "podman", arguments...)
	return err
}

func (h *harness) runConcurrent(ctx context.Context) error {
	var wait sync.WaitGroup
	errorsChannel := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := h.runOne(ctx); err != nil {
				errorsChannel <- err
			}
		}()
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			return err
		}
	}
	return nil
}

func (h *harness) expectFailure(ctx context.Context, expected string) error {
	err := h.runOne(ctx)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(expected)) {
		return fmt.Errorf("initializer error %v did not contain %q", err, expected)
	}
	return nil
}
