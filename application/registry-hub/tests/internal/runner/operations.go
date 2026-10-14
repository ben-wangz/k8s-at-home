package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/environment"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func (runtime *Runtime) Check(ctx context.Context, build bool) (environment.PreflightResult, error) {
	return runtime.Environment.Check(ctx, build)
}

func (runtime *Runtime) Inspect(ctx context.Context, id string) error {
	if err := testkit.ValidateRunID(id); err != nil {
		return err
	}
	contextName, err := runtime.Environment.CurrentContext(ctx)
	if err != nil {
		return err
	}
	registration, _, err := runtime.Environment.GetRegistration(ctx, id)
	if err != nil {
		return err
	}
	if err := environment.ValidateRegistration(registration, id, contextName); err != nil {
		return err
	}
	fmt.Printf("Run ID: %s\nScenario: %s\nContext: %s\nNamespace: %s\nRelease: %s\nState: %s\n", id, registration.ScenarioID, contextName, environment.Namespace, registration.Release, registration.State)
	stateDir := scenarioStateDir(runtime.StateRoot, registration.ScenarioID, id)
	if report, err := readRecord(stateDir); err == nil {
		fmt.Printf("Report: %s\n", filepath.Join(stateDir, "run.json"))
		if report.ScenarioValuesSHA256 != "" {
			fmt.Printf("Scenario values SHA-256: %s\n", report.ScenarioValuesSHA256)
		}
		if report.EnvironmentValuesSHA256 != "" {
			fmt.Printf("Environment values SHA-256: %s\n", report.EnvironmentValuesSHA256)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if registration.ImageDigest != "" {
		fmt.Printf("Backend digest: %s\n", registration.ImageDigest)
	}
	for _, resource := range []string{"pods", "persistentvolumeclaims"} {
		result, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "get", resource, "-l", "app.kubernetes.io/instance="+registration.Release, "-o", "wide")
		if err != nil {
			return err
		}
		fmt.Print(string(result.Stdout))
	}
	if _, err := testkit.Command(ctx, "helm", "status", registration.Release, "--namespace", environment.Namespace); err != nil {
		return err
	}
	return nil
}

func (runtime *Runtime) Destroy(ctx context.Context, id string) error {
	if err := testkit.ValidateRunID(id); err != nil {
		return err
	}
	contextName, err := runtime.Environment.CurrentContext(ctx)
	if err != nil {
		return err
	}
	registration, _, err := runtime.Environment.GetRegistration(ctx, id)
	if err != nil {
		if testkit.IsNotFound(err) {
			if verifyErr := runtime.Environment.VerifyAbsent(ctx, id); verifyErr != nil {
				return verifyErr
			}
			fmt.Printf("Test environment %s is already absent.\n", id)
			return nil
		}
		return err
	}
	if err := environment.ValidateRegistration(registration, id, contextName); err != nil {
		return err
	}
	stateDir := scenarioStateDir(runtime.StateRoot, registration.ScenarioID, id)
	lock, err := acquireLocalLock(stateDir)
	if err != nil {
		return err
	}
	defer releaseLocalLock(lock)
	owner, err := leaseOwner()
	if err != nil {
		return err
	}
	registration, identity, err := runtime.Environment.ClaimRegistration(ctx, id, owner, contextName)
	if err != nil {
		return err
	}
	leaseContext, _, stopLease := runtime.startLease(ctx, id, owner)
	reservationPresent := true
	defer func() {
		stopLease()
		if !reservationPresent {
			return
		}
		releaseContext, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = runtime.Environment.ReleaseLease(releaseContext, id, owner)
	}()
	report, err := runtime.loadOrCreateReport(stateDir, registration)
	if err != nil {
		return err
	}
	report.State = "cleaning"
	if err := saveRecord(stateDir, report); err != nil {
		return err
	}
	registration.State = "cleaning"
	if err := runtime.Environment.UpdateRegistration(leaseContext, registration); err != nil {
		return err
	}
	resources, err := runtime.Environment.Destroy(leaseContext, registration, identity.UID)
	if err != nil {
		report.State = "cleanup_failed"
		report.CleanupError = err.Error()
		report.Failure = err.Error()
		_ = saveRecord(stateDir, report)
		registration.State = "cleanup_failed"
		_ = runtime.Environment.UpdateRegistration(context.Background(), registration)
		return err
	}
	reservationPresent = false
	stopLease()
	now := time.Now().UTC()
	report.State = "destroyed"
	report.FinishedAt = &now
	report.Resources = resources
	report.CleanupError = ""
	report.Failure = ""
	if err := saveRecord(stateDir, report); err != nil {
		return err
	}
	fmt.Printf("Destroyed test environment %s.\n", id)
	return nil
}

func (runtime *Runtime) loadOrCreateReport(stateDir string, registration environment.Registration) (testkit.RunRecord, error) {
	report, err := readRecord(stateDir)
	if err == nil {
		if report.ID != registration.ID || report.ScenarioID != registration.ScenarioID || report.Release != registration.Release || report.Fullname != registration.Fullname {
			return testkit.RunRecord{}, fmt.Errorf("local report does not match test environment ownership")
		}
		return report, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return testkit.RunRecord{}, err
	}
	return testkit.RunRecord{
		ID: registration.ID, ScenarioID: registration.ScenarioID, Namespace: environment.Namespace, KubeContext: registration.KubeContext,
		Release: registration.Release, Fullname: registration.Fullname,
		ConfigSHA256: registration.ConfigSHA256, SourceSHA256: registration.SourceSHA256,
		ScenarioFixtureSHA256: registration.ScenarioFixtureSHA256, ScenarioValuesSHA256: registration.ScenarioValuesSHA256,
		EnvironmentValuesSHA256: registration.EnvironmentValuesSHA256,
		ImageReference:          registration.ImageReference, ImageDigest: registration.ImageDigest,
		ValuesFile: registration.ValuesFile, StartedAt: registration.StartedAt,
		State: registration.State, Cases: map[string]testkit.CaseResult{},
	}, nil
}
