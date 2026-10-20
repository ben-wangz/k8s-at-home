package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ben-wangz/clash-tests/internal/environment"
	"github.com/ben-wangz/clash-tests/internal/testkit"
)

func lifecycleCommands(ctx context.Context, app, directory string, record *environment.Record) ([]testkit.Evidence, error) {
	id := environment.NewID()
	script := filepath.Join(app, "tests/features/minimal/run.sh")
	variables := []string{
		"CLASH_TEST_CONTEXT=" + record.Context,
		"CLASH_TEST_NAMESPACE=" + record.Namespace,
		"CLASH_TEST_FIXTURE_IMAGE=" + record.FixtureImage,
		"CLASH_TEST_MIHOMO_IMAGE=" + record.MihomoImage,
	}
	var evidence []testkit.Evidence
	invoke := func(name string, arguments ...string) ([]byte, error) {
		output, commandErr := testkit.Command(ctx, app, variables, script, arguments...)
		path := filepath.Join(directory, name+".txt")
		if err := os.WriteFile(path, output, 0600); err != nil {
			return output, err
		}
		evidence = append(evidence, testkit.Evidence{ID: name, Path: path})
		if commandErr != nil {
			return output, fmt.Errorf("%s failed; see %s: %w", name, path, commandErr)
		}
		return output, nil
	}
	inspect := func(name, expectedState string) (*environment.Record, error) {
		output, err := invoke(name, "environment", "inspect", "--id", id)
		if err != nil {
			return nil, err
		}
		var current environment.Record
		if err := json.Unmarshal(output, &current); err != nil {
			return nil, fmt.Errorf("%s returned an invalid run record: %w", name, err)
		}
		if current.ID != id || current.State != expectedState {
			return nil, fmt.Errorf("%s returned state %q for run %q, expected %q", name, current.State, current.ID, expectedState)
		}
		if expectedState == "failed" {
			failedInstallation := false
			for _, result := range current.Cases {
				if result.ID == "installation" && result.Status == "failed" {
					failedInstallation = true
				}
			}
			if current.Failure == "" || !failedInstallation {
				return nil, fmt.Errorf("%s did not preserve the failed installation result", name)
			}
		}
		return &current, nil
	}
	invokeExpectedFailure := func(name string, arguments ...string) error {
		_, err := invoke(name, arguments...)
		if err == nil {
			return fmt.Errorf("%s unexpectedly succeeded", name)
		}
		return nil
	}

	if _, err := invoke("environment-create", "environment", "create", "--scenario", record.Scenario, "--id", id); err != nil {
		return nil, err
	}
	inner, err := inspect("environment-inspect-ready", "environment-ready")
	if err != nil {
		return nil, err
	}
	scaleDown, err := scaleLifecycleDeployment(ctx, app, directory, inner, 0)
	if err != nil {
		return nil, err
	}
	evidence = append(evidence, scaleDown)
	if err := invokeExpectedFailure("environment-reuse-failed", "run", "--id", id, "--reuse", "--case", "installation"); err != nil {
		return nil, err
	}
	if _, err := inspect("environment-inspect-failed", "failed"); err != nil {
		return nil, err
	}
	scaleUp, err := scaleLifecycleDeployment(ctx, app, directory, inner, 1)
	if err != nil {
		return nil, err
	}
	evidence = append(evidence, scaleUp)
	if _, err := invoke("environment-reuse", "run", "--id", id, "--reuse", "--case", "installation"); err != nil {
		return nil, err
	}
	if _, err := inspect("environment-inspect-passed", "passed"); err != nil {
		return nil, err
	}
	if _, err := invoke("environment-destroy", "environment", "destroy", "--id", id); err != nil {
		return nil, err
	}
	if _, err := inspect("environment-inspect-destroyed", "destroyed"); err != nil {
		return nil, err
	}
	return evidence, nil
}

func scaleLifecycleDeployment(ctx context.Context, app, directory string, record *environment.Record, replicas int) (testkit.Evidence, error) {
	name := "environment-scale-down"
	if replicas > 0 {
		name = "environment-scale-up"
	}
	output, err := testkit.Command(ctx, app, nil, "kubectl", "--context", record.Context, "--namespace", record.Namespace, "scale", "deployment/"+record.Release, "--replicas="+strconv.Itoa(replicas))
	path := filepath.Join(directory, name+".txt")
	if writeErr := os.WriteFile(path, output, 0600); writeErr != nil {
		return testkit.Evidence{}, writeErr
	}
	evidence := testkit.Evidence{ID: name, Path: path}
	if err != nil {
		return evidence, fmt.Errorf("%s failed; see %s: %w", name, path, err)
	}
	waitTimeout := 90 * time.Second
	if replicas > 0 {
		waitTimeout = 240 * time.Second
	}
	waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	for {
		status, err := testkit.Command(waitCtx, app, nil, "kubectl", "--context", record.Context, "--namespace", record.Namespace, "get", "deployment/"+record.Release, "-o", "jsonpath={.status.replicas}")
		if err != nil {
			return evidence, fmt.Errorf("cannot read deployment replica count: %w: %s", err, status)
		}
		actual := strings.TrimSpace(string(status))
		if actual == strconv.Itoa(replicas) || replicas == 0 && actual == "" {
			break
		}
		select {
		case <-waitCtx.Done():
			return evidence, fmt.Errorf("deployment %s did not reach %d replicas", record.Release, replicas)
		case <-time.After(time.Second):
		}
	}
	if replicas > 0 {
		output, err := testkit.Command(waitCtx, app, nil, "kubectl", "--context", record.Context, "--namespace", record.Namespace, "rollout", "status", "deployment/"+record.Release, "--timeout=180s")
		if err != nil {
			return evidence, fmt.Errorf("deployment rollout did not recover: %w: %s", err, output)
		}
	}
	return evidence, nil
}
