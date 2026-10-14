package runner

import (
	"context"
	"errors"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/internal/environment"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"os"
	"path/filepath"
	"time"
)

func (runtime *Runtime) openRun(ctx context.Context, options RunOptions, scenario ScenarioDefinition, id, stateDir string, plan Plan, preflight environment.PreflightResult, configHash string, source environment.SourceIdentity) (testkit.RunRecord, environment.Registration, string, string, error) {
	owner, err := leaseOwner()
	if err != nil {
		return testkit.RunRecord{}, environment.Registration{}, "", "", err
	}
	contextName := preflight.KubeContext
	release, err := environment.ReleaseName(scenario.ID, id)
	if err != nil {
		return testkit.RunRecord{}, environment.Registration{}, "", "", err
	}
	fullname := release + "-registry-hub"
	reportPath := filepath.Join(stateDir, "run.json")
	if options.Reuse {
		return runtime.resumeRun(ctx, scenario.ID, id, stateDir, reportPath, owner, contextName, configHash, source)
	}
	if _, err := os.Stat(reportPath); err == nil {
		return testkit.RunRecord{}, environment.Registration{}, "", "", fmt.Errorf("run ID %s already has a local report; use --reuse or select a new ID", id)
	} else if !errors.Is(err, os.ErrNotExist) {
		return testkit.RunRecord{}, environment.Registration{}, "", "", err
	}
	exists, err := runtime.Environment.ReleaseExists(ctx, release)
	if err != nil {
		return testkit.RunRecord{}, environment.Registration{}, "", "", err
	}
	if exists {
		return testkit.RunRecord{}, environment.Registration{}, "", "", fmt.Errorf("Helm release %s already exists; refusing to adopt it", release)
	}
	now := time.Now().UTC()
	registration := environment.Registration{
		ID: id, ScenarioID: scenario.ID, Release: release, Fullname: fullname, KubeContext: contextName,
		ConfigSHA256: configHash, SourceSHA256: source.SHA256, State: "preparing",
		ActiveOwner: owner, LeaseUntil: now.Add(90 * time.Second), StartedAt: now,
	}
	identity, err := runtime.Environment.Reserve(ctx, registration)
	if err != nil {
		return testkit.RunRecord{}, environment.Registration{}, "", "", err
	}
	report := testkit.RunRecord{
		ID: id, ScenarioID: scenario.ID, Namespace: environment.Namespace, KubeContext: contextName,
		PreflightTools: append([]string(nil), preflight.AvailableTools...),
		PreflightAt:    now,
		Release:        release, Fullname: fullname, ConfigSHA256: configHash,
		SourceSHA256: source.SHA256, GitRevision: source.Revision,
		WorktreeStatus: source.Status, StartedAt: now, State: "preparing",
		ExplicitCases: plan.Explicit, Plan: plan.Order, Cases: map[string]testkit.CaseResult{},
		Resources: []testkit.ResourceIdentity{identity},
	}
	if err := saveRecord(stateDir, report); err != nil {
		return report, registration, identity.UID, owner, err
	}
	return report, registration, identity.UID, owner, nil
}

func (runtime *Runtime) resumeRun(ctx context.Context, scenarioID, id, stateDir, reportPath, owner, contextName, configHash string, source environment.SourceIdentity) (testkit.RunRecord, environment.Registration, string, string, error) {
	registration, identity, err := runtime.Environment.GetRegistration(ctx, id)
	if err != nil {
		return testkit.RunRecord{}, environment.Registration{}, "", "", err
	}
	if err := environment.ValidateRegistration(registration, id, contextName); err != nil {
		return testkit.RunRecord{}, environment.Registration{}, "", "", err
	}
	if registration.ScenarioID != scenarioID || registration.ConfigSHA256 != configHash || registration.SourceSHA256 != source.SHA256 {
		return testkit.RunRecord{}, environment.Registration{}, "", "", fmt.Errorf("run ID %s configuration or source changed; use a new ID", id)
	}
	report, err := readRecord(stateDir)
	if err != nil {
		return testkit.RunRecord{}, environment.Registration{}, "", "", fmt.Errorf("cannot resume without the protected local run report: %w", err)
	}
	if report.ID != id || report.ScenarioID != scenarioID || report.ConfigSHA256 != configHash || report.SourceSHA256 != source.SHA256 || report.KubeContext != contextName {
		return testkit.RunRecord{}, environment.Registration{}, "", "", fmt.Errorf("local run report identity does not match the preserved environment")
	}
	if report.Release != registration.Release || report.Fullname != registration.Fullname {
		return testkit.RunRecord{}, environment.Registration{}, "", "", fmt.Errorf("local run report release identity does not match the registration")
	}
	if identity.Name != environment.RegistrationName(id) || identity.UID == "" || !containsResource(report.Resources, identity) {
		return testkit.RunRecord{}, environment.Registration{}, "", "", fmt.Errorf("test ID registration UID does not match the local run report")
	}
	switch report.State {
	case "failed", "interrupted", "environment-ready", "preparing", "running":
	default:
		return testkit.RunRecord{}, environment.Registration{}, "", "", fmt.Errorf("run ID %s is in state %q and cannot be resumed", id, report.State)
	}
	registration, claimedIdentity, err := runtime.Environment.ClaimRegistration(ctx, id, owner, contextName)
	if err != nil {
		return testkit.RunRecord{}, environment.Registration{}, "", "", err
	}
	if claimedIdentity.UID != identity.UID {
		return testkit.RunRecord{}, environment.Registration{}, "", "", fmt.Errorf("test ID registration changed while claiming its lease")
	}
	identity = claimedIdentity
	report.State = "preparing"
	report.Failure = ""
	report.FinishedAt = nil
	if err := saveRecord(stateDir, report); err != nil {
		return report, registration, identity.UID, owner, err
	}
	return report, registration, identity.UID, owner, nil
}

func (runtime *Runtime) prepareEnvironment(ctx context.Context, leaseErrors <-chan error, baseValues, id, stateDir string, source environment.SourceIdentity, requestedImage string, report *testkit.RunRecord, registration *environment.Registration) error {
	if err := checkLease(ctx, leaseErrors); err != nil {
		return err
	}
	var image environment.ImageBuild
	var err error
	if requestedImage != "" {
		image, err = runtime.Environment.UseImageReference(requestedImage)
		if err == nil && report.ImageReference != "" && image.Reference != report.ImageReference {
			err = fmt.Errorf("requested backend image does not match the preserved run")
		}
	} else if registration.ImageReference != "" {
		image, err = runtime.Environment.UseImageReference(registration.ImageReference)
	} else if report.ImageReference != "" {
		image, err = runtime.Environment.UseImageReference(report.ImageReference)
	} else {
		image, err = runtime.Environment.BuildAndPush(ctx, id, stateDir)
	}
	if err != nil {
		return err
	}
	valuesPath := filepath.Join(stateDir, "environment-values.yaml")
	if err := runtime.Environment.WriteValues(valuesPath, image); err != nil {
		return err
	}
	valuesHash, err := hashFile(valuesPath)
	if err != nil {
		return err
	}
	if report.EnvironmentValuesSHA256 != "" && report.EnvironmentValuesSHA256 != valuesHash ||
		registration.EnvironmentValuesSHA256 != "" && registration.EnvironmentValuesSHA256 != valuesHash {
		return fmt.Errorf("environment values changed for run ID %s", id)
	}
	if report.ImageReference != "" && report.ImageReference != image.Reference {
		return fmt.Errorf("preserved environment image digest does not match the local report")
	}
	report.ImageReference = image.Reference
	report.ImageDigest = image.Digest
	report.ValuesFile = valuesPath
	report.EnvironmentValuesSHA256 = valuesHash
	report.SourceSHA256 = source.SHA256
	if requestedImage != "" {
		report.Mode = "prebuilt-image"
	} else if report.Mode == "" {
		report.Mode = "build-and-push"
	}
	registration.ImageReference = image.Reference
	registration.ImageDigest = image.Digest
	registration.ValuesFile = valuesPath
	registration.EnvironmentValuesSHA256 = valuesHash
	registration.State = "deploying"
	if err := saveRecord(stateDir, *report); err != nil {
		return err
	}
	if err := runtime.Environment.UpdateRegistration(ctx, *registration); err != nil {
		return err
	}
	if err := runtime.Environment.Deploy(ctx, report.Release, baseValues, valuesPath); err != nil {
		return err
	}
	return nil
}

func (runtime *Runtime) makeCaseContext(ctx context.Context, scenario ScenarioDefinition, id, stateDir string, report *testkit.RunRecord, preflight environment.PreflightResult) (testkit.CaseContext, string, error) {
	client, err := runtime.Environment.Service(ctx, report.Fullname, "registry")
	if err != nil {
		return testkit.CaseContext{}, "", err
	}
	admin, err := runtime.Environment.Service(ctx, report.Fullname+"-admin", "admin")
	if err != nil {
		return testkit.CaseContext{}, "", err
	}
	value := testkit.CaseContext{
		RunID: id, ScenarioID: scenario.ID, Namespace: environment.Namespace, Release: report.Release,
		Fullname: report.Fullname, KubeContext: report.KubeContext,
		ClientURL: "http://" + client.Address, AdminURL: "http://" + admin.Address,
		ValuesFile: report.ValuesFile, ChartDir: runtime.Environment.ChartDir,
		BaseValues:     filepath.Join(stateDir, "scenario-values.yaml"),
		ImageReference: report.ImageReference, ImageDigest: report.ImageDigest, StateDir: stateDir,
	}
	path := filepath.Join(stateDir, "context.json")
	if err := testkit.WriteJSONAtomic(path, value); err != nil {
		return testkit.CaseContext{}, "", err
	}
	return value, path, nil
}
