package runner

import (
	"context"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/internal/environment"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"os"
	"path/filepath"
	"time"
)

func (runtime *Runtime) Run(ctx context.Context, options RunOptions) error {
	scenario, err := runtime.scenario(options.Scenario)
	if err != nil {
		return err
	}
	options.Scenario = scenario.ID
	if options.Reuse && options.ID == "" {
		return fmt.Errorf("--reuse requires an explicit --id")
	}
	id := options.ID
	if id == "" {
		generated, err := testkit.GenerateRunID()
		if err != nil {
			return err
		}
		id = generated
	}
	if err := testkit.ValidateRunID(id); err != nil {
		return err
	}
	fmt.Printf("Run ID: %s\n", id)
	var plan Plan
	if !options.CreateOnly {
		plan, err = runtime.PlanScenario(options.Scenario, options.Selected)
		if err != nil {
			return err
		}
		printPlan(plan)
	}
	needsPodman := options.Image == "" && !options.Reuse
	if !options.CreateOnly {
		for _, caseID := range plan.Order {
			if caseID == "initializer-faults" || caseID == "client-interop" || caseID == "podman-upstream-pull" {
				needsPodman = true
				break
			}
		}
	}
	preflight, err := runtime.Environment.Check(ctx, needsPodman)
	if err != nil {
		return err
	}
	configPath := filepath.Join(runtime.Environment.AppDir, scenario.ConfigPath)
	configHash, err := hashFile(configPath)
	if err != nil {
		return err
	}
	if err := validateScenarioConfig(configPath, scenario); err != nil {
		return err
	}
	source, err := runtime.Environment.Source(ctx)
	if err != nil {
		return err
	}
	stateDir := scenarioStateDir(runtime.StateRoot, scenario.ID, id)
	lock, err := acquireLocalLock(stateDir)
	if err != nil {
		return err
	}
	defer releaseLocalLock(lock)
	report, registration, registrationUID, owner, err := runtime.openRun(ctx, options, scenario, id, stateDir, plan, preflight, configHash, source)
	if err != nil {
		return err
	}
	leaseContext, leaseErrors, stopLease := runtime.startLease(ctx, id, owner)
	defer stopLease()
	reservationPresent := true
	defer func() {
		if !reservationPresent {
			return
		}
		releaseContext, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = runtime.Environment.ReleaseLease(releaseContext, id, owner)
	}()
	preserve := func(cause error, state string) error {
		report.State = state
		report.Failure = cause.Error()
		now := time.Now().UTC()
		report.FinishedAt = &now
		_ = saveRecord(stateDir, report)
		updateContext, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		registration.State = state
		registration.ActiveOwner = owner
		_ = runtime.Environment.UpdateRegistration(updateContext, registration)
		if state == "cleanup_failed" {
			report.CleanupError = cause.Error()
			_ = saveRecord(stateDir, report)
			fmt.Fprintf(os.Stderr, "Cases passed; cleanup failed. Inspect or destroy test ID %s.\n", id)
		}
		printSummary(report)
		return cause
	}
	expectedFixtureHash := report.ScenarioFixtureSHA256
	if expectedFixtureHash == "" {
		expectedFixtureHash = registration.ScenarioFixtureSHA256
	}
	scenarioValues, fixtureHash, valuesHash, err := runtime.prepareScenario(leaseContext, scenario, id, report.Fullname, stateDir, expectedFixtureHash, &report)
	if err != nil {
		return preserve(err, stateForError(leaseContext, err))
	}
	if report.ScenarioFixtureSHA256 != "" && report.ScenarioFixtureSHA256 != fixtureHash ||
		registration.ScenarioFixtureSHA256 != "" && registration.ScenarioFixtureSHA256 != fixtureHash {
		return preserve(fmt.Errorf("scenario fixture changed for run ID %s", id), "failed")
	}
	if report.ScenarioValuesSHA256 != "" && report.ScenarioValuesSHA256 != valuesHash ||
		registration.ScenarioValuesSHA256 != "" && registration.ScenarioValuesSHA256 != valuesHash {
		return preserve(fmt.Errorf("scenario values changed for run ID %s", id), "failed")
	}
	report.ScenarioFixtureSHA256 = fixtureHash
	report.ScenarioValuesSHA256 = valuesHash
	registration.ScenarioFixtureSHA256 = fixtureHash
	registration.ScenarioValuesSHA256 = valuesHash
	if err := saveRecord(stateDir, report); err != nil {
		return preserve(err, "failed")
	}
	if err := runtime.Environment.UpdateRegistration(leaseContext, registration); err != nil {
		return preserve(err, "failed")
	}
	if err := runtime.prepareEnvironment(leaseContext, leaseErrors, scenarioValues, id, stateDir, source, options.Image, &report, &registration); err != nil {
		return preserve(err, stateForError(leaseContext, err))
	}
	if err := copyFile(configPath, filepath.Join(stateDir, "config.yaml")); err != nil {
		return preserve(err, "failed")
	}
	if options.CreateOnly {
		report.State = "environment-ready"
		if err := saveRecord(stateDir, report); err != nil {
			return preserve(err, "failed")
		}
		registration.State = "environment-ready"
		if err := runtime.Environment.UpdateRegistration(leaseContext, registration); err != nil {
			return preserve(err, "failed")
		}
		fmt.Printf("Environment %s is ready in namespace %s.\n", id, environment.Namespace)
		return nil
	}
	caseContext, contextPath, err := runtime.makeCaseContext(leaseContext, scenario, id, stateDir, &report, preflight)
	if err != nil {
		return preserve(err, "failed")
	}
	report.State = "running"
	report.Attempt++
	report.ExplicitCases = plan.Explicit
	report.Plan = plan.Order
	report.Cases = make(map[string]testkit.CaseResult, len(plan.Order))
	caseIndex, err := runtime.caseIndexForScenario(options.Scenario)
	if err != nil {
		return preserve(err, "failed")
	}
	for _, caseID := range plan.Order {
		report.Cases[caseID] = testkit.CaseResult{
			Status: "not-run", Requirements: append([]string(nil), caseIndex[caseID].Definition.Requirements...),
		}
	}
	if err := saveRecord(stateDir, report); err != nil {
		return preserve(err, "failed")
	}
	registration.State = "running"
	if err := runtime.Environment.UpdateRegistration(leaseContext, registration); err != nil {
		return preserve(err, "failed")
	}
	failedCase, runErr := runtime.executeCases(leaseContext, leaseErrors, id, stateDir, contextPath, caseContext, caseIndex, plan, &report)
	if runErr != nil {
		applyRemainingStates(report.Cases, plan.Order, caseIndex, failedCase, runErr)
		return preserve(runErr, stateForError(leaseContext, runErr))
	}
	if err := runtime.cleanup(leaseContext, id, stateDir, registration, registrationUID, &report); err != nil {
		return preserve(err, "cleanup_failed")
	}
	reservationPresent = false
	report.State = "passed"
	report.Failure = ""
	now := time.Now().UTC()
	report.FinishedAt = &now
	if err := saveRecord(stateDir, report); err != nil {
		return err
	}
	printSummary(report)
	return nil
}
