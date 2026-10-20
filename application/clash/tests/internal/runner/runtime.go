package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ben-wangz/clash-tests/internal/environment"
	"github.com/ben-wangz/clash-tests/internal/testkit"
)

type Options struct {
	ID, Scenario            string
	Selected                []string
	Reuse, Mock, CreateOnly bool
}

func Run(ctx context.Context, app string, definitions []testkit.Definition, scenario Scenario, options Options) error {
	plan, err := ScenarioPlan(definitions, scenario, options.Selected)
	if err != nil {
		return err
	}
	if options.ID == "" {
		if options.Reuse {
			return fmt.Errorf("--reuse requires --id")
		}
		options.ID = environment.NewID()
	}
	unlock, err := environment.LocalLock(options.ID)
	if err != nil {
		return err
	}
	defer unlock()
	env := environment.New(app)
	needsCluster := options.CreateOnly
	for _, item := range plan {
		if (item.Mode == "browser" || item.Mode == "lifecycle") && !options.Mock {
			needsCluster = true
		}
	}
	if needsCluster {
		if err := env.Check(ctx, scenario.ID); err != nil {
			return err
		}
	}
	source, err := environment.SourceIdentity(app)
	if err != nil {
		return err
	}
	configuration, err := environment.ConfigurationIdentity(app, scenario.ID)
	if err != nil {
		return err
	}
	directory := filepath.Join(environment.StateRoot(), scenario.ID, options.ID)
	record := &environment.Record{ID: options.ID, Scenario: scenario.ID, Namespace: env.Namespace, Context: env.Context, Release: "clash-test-" + options.ID, State: "preparing", SourceSHA: source, ConfigurationSHA: configuration, Started: time.Now().UTC(), Mode: "local"}
	if needsCluster {
		record.Mode = "integration"
	} else if options.Mock {
		record.Mode = "mock"
	}
	if options.Reuse {
		directory, record, err = environment.Load(options.ID)
		if err != nil {
			return err
		}
		if err := validateReuse(record, scenario.ID, source, configuration, env.Context, env.Namespace); err != nil {
			return err
		}
	} else {
		existing, err := filepath.Glob(filepath.Join(environment.StateRoot(), "*", options.ID, "run.json"))
		if err != nil {
			return err
		}
		if len(existing) > 0 {
			return fmt.Errorf("run ID already exists; use --reuse or choose a new ID")
		}
		if err := os.MkdirAll(directory, 0700); err != nil {
			return err
		}
	}
	record.Plan = nil
	for _, definition := range plan {
		record.Plan = append(record.Plan, definition.ID)
	}
	record.Tools = toolVersions(ctx, app)
	if err := environment.Save(directory, record); err != nil {
		return err
	}
	fmt.Printf("Run %s (%s): %s\n", record.ID, record.Mode, directory)
	work, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if needsCluster {
		release, err := env.Lease(work, record.ID, cancel)
		if err != nil {
			record.State, record.Failure = "failed", err.Error()
			finished := time.Now().UTC()
			record.Finished = &finished
			if saveErr := environment.Save(directory, record); saveErr != nil {
				return errors.Join(err, saveErr)
			}
			return fmt.Errorf("run %s could not acquire its environment lease: %w", record.ID, err)
		}
		defer release()
		if !options.Reuse {
			if err := env.Images(work, directory, record); err != nil {
				return preserve(env, directory, record, withContextCause(work, err))
			}
			if err := env.Fixtures(work, directory, record); err != nil {
				return preserve(env, directory, record, withContextCause(work, err))
			}
			if err := env.Deploy(work, directory, record); err != nil {
				return preserve(env, directory, record, withContextCause(work, err))
			}
		} else {
			if err := env.Collect(work, record); err != nil {
				return err
			}
		}
	}
	if options.CreateOnly {
		record.State = "environment-ready"
		return environment.Save(directory, record)
	}
	if err := writeBrowserContext(directory, record); err != nil {
		return preserve(env, directory, record, err)
	}
	record.Cases, record.Plan = nil, nil
	for _, definition := range plan {
		record.Plan = append(record.Plan, definition.ID)
	}
	record.State = "running"
	failed := executePlan(work, app, directory, record, plan)
	if cause := context.Cause(work); cause != nil {
		failed = errors.Join(failed, cause)
	}
	finished := time.Now().UTC()
	record.Finished = &finished
	if failed != nil {
		return preserve(env, directory, record, failed)
	}
	if needsCluster {
		if err := env.Destroy(work, directory, record); err != nil {
			record.State, record.CleanupError = "cleanup-failed", err.Error()
			_ = environment.Save(directory, record)
			return fmt.Errorf("checks passed but cleanup failed: %w", err)
		}
	}
	record.State, record.Failure = "passed", ""
	fmt.Printf("Passed %d cases; report: %s\n", len(plan), filepath.Join(directory, "run.json"))
	return environment.Save(directory, record)
}

func validateReuse(record *environment.Record, scenario, source, configuration, kubeContext, namespace string) error {
	if record.ID == "" || record.Release != "clash-test-"+record.ID {
		return fmt.Errorf("preserved run identity is invalid")
	}
	if record.Scenario != scenario || record.SourceSHA != source || record.ConfigurationSHA != configuration || record.Context != kubeContext || record.Namespace != namespace {
		return fmt.Errorf("preserved run does not match scenario, source, configuration, or environment; use a new ID")
	}
	if record.State == "passed" || record.State == "destroyed" || record.Mode != "integration" {
		return fmt.Errorf("only a preserved integration environment can be reused")
	}
	return nil
}

func withContextCause(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil {
		return errors.Join(err, cause)
	}
	return err
}

func executePlan(ctx context.Context, app, directory string, record *environment.Record, plan []testkit.Definition) error {
	statuses := make(map[string]string)
	var failures []error
	for _, definition := range plan {
		result := testkit.CaseResult{ID: definition.ID, Status: "passed", Started: time.Now().UTC()}
		blocked := ctx.Err() != nil
		for _, dependency := range definition.DependsOn {
			if statuses[dependency] != "passed" {
				blocked = true
			}
		}
		if blocked {
			result.Status, result.Error = "blocked", "A prerequisite failed or the run was interrupted."
		} else {
			deadline, cancel := context.WithTimeout(ctx, time.Duration(definition.TimeoutSeconds)*time.Second)
			checks, err := runCase(deadline, app, directory, record, definition)
			cancel()
			result.Checks = checks
			if err == nil {
				err = ValidateResults(definition, checks, record.Mode)
			}
			if err != nil {
				result.Status, result.Error = "failed", err.Error()
				failures = append(failures, fmt.Errorf("%s: %w", definition.ID, err))
			}
		}
		result.Finished = time.Now().UTC()
		statuses[definition.ID] = result.Status
		record.Cases = append(record.Cases, result)
		if err := environment.Save(directory, record); err != nil {
			return err
		}
		fmt.Printf("%-24s %s\n", definition.ID, result.Status)
	}
	if ctx.Err() != nil {
		failures = append(failures, ctx.Err())
	}
	return errors.Join(failures...)
}

func preserve(env environment.Environment, directory string, record *environment.Record, cause error) error {
	record.State, record.Failure = "failed", cause.Error()
	if record.Finished == nil {
		finished := time.Now().UTC()
		record.Finished = &finished
	}
	if record.Mode == "integration" {
		env.Diagnostics(directory, record)
	}
	_ = environment.Save(directory, record)
	return fmt.Errorf("run %s failed; retained report and environment at %s: %w", record.ID, directory, cause)
}

func writeBrowserContext(directory string, record *environment.Record) error {
	return testkit.WriteJSON(filepath.Join(directory, "browser-context.json"), map[string]any{
		"mode": record.Mode, "url": record.FrontendURL, "controllerURL": record.ControllerURL, "proxyURL": record.ProxyURL,
		"trafficURL": record.TrafficURL, "secretFile": filepath.Join(directory, "controller-secret"),
		"namespace": record.Namespace, "kubeContext": record.Context, "deployment": record.Release,
	})
}

func toolVersions(ctx context.Context, app string) map[string]string {
	versions := make(map[string]string)
	for tool, args := range map[string][]string{"node": {"--version"}, "npm": {"--version"}, "go": {"version"}, "helm": {"version", "--short"}, "podman": {"--version"}} {
		if output, err := testkit.Command(ctx, app, nil, tool, args...); err == nil {
			versions[tool] = strings.TrimSpace(string(output))
		}
	}
	return versions
}
