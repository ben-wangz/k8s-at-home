package runner

import (
	"context"
	"errors"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/internal/environment"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func (runtime *Runtime) executeCases(ctx context.Context, leaseErrors <-chan error, id, stateDir, contextPath string, shared testkit.CaseContext, cases map[string]testkit.Case, plan Plan, report *testkit.RunRecord) (string, error) {
	outputs := make(map[string]testkit.CaseResultFile, len(plan.Order))
	for _, caseID := range plan.Order {
		item := cases[caseID]
		started := time.Now().UTC()
		result := testkit.CaseResult{
			Status: "running", StartedAt: started,
			Requirements: append([]string(nil), item.Definition.Requirements...),
		}
		report.Cases[caseID] = result
		failCurrent := func(failure error) (string, error) {
			finished := time.Now().UTC()
			result.Status = "failed"
			if errors.Is(failure, context.DeadlineExceeded) {
				result.Status = "timeout"
			}
			result.FinishedAt = finished
			result.Duration = finished.Sub(started)
			result.Error = failure.Error()
			report.Cases[caseID] = result
			_ = saveRecord(stateDir, *report)
			return caseID, fmt.Errorf("case %s %s: %w", caseID, result.Status, failure)
		}
		if err := checkLease(ctx, leaseErrors); err != nil {
			return failCurrent(err)
		}
		if err := saveRecord(stateDir, *report); err != nil {
			return caseID, err
		}
		outputDir := filepath.Join(stateDir, "cases", caseID, fmt.Sprintf("attempt-%03d", report.Attempt))
		if err := testkit.EnsurePrivateDir(outputDir); err != nil {
			return failCurrent(err)
		}
		caseContext := shared
		caseContext.CaseID = caseID
		caseContext.OutputDir = outputDir
		clientService, err := runtime.Environment.Service(ctx, shared.Fullname, "registry")
		if err != nil {
			return failCurrent(err)
		}
		adminService, err := runtime.Environment.Service(ctx, shared.Fullname+"-admin", "admin")
		if err != nil {
			return failCurrent(err)
		}
		caseContext.ClientURL = "http://" + clientService.Address
		caseContext.AdminURL = "http://" + adminService.Address
		caseContext.DependencyResults = make(map[string]testkit.CaseResultFile, len(item.Definition.DependsOn))
		for _, dependency := range item.Definition.DependsOn {
			output, exists := outputs[dependency]
			if !exists {
				return failCurrent(fmt.Errorf("case has no successful result from dependency %s", dependency))
			}
			caseContext.DependencyResults[dependency] = output
		}
		if err := testkit.WriteJSONAtomic(contextPath, caseContext); err != nil {
			return failCurrent(err)
		}
		logPath := filepath.Join(outputDir, "case.log")
		err = runCaseProcess(ctx, item, caseContext, contextPath, logPath)
		var caseOutput testkit.CaseResultFile
		if err == nil {
			if outputErr := testkit.ReadJSON(filepath.Join(outputDir, "result.json"), &caseOutput); outputErr != nil {
				err = fmt.Errorf("read case %s structured result: %w", caseID, outputErr)
			} else if caseOutput.CaseID != caseID || caseOutput.Status != "passed" {
				err = fmt.Errorf("case %s produced an invalid structured result", caseID)
			} else if checkErr := testkit.ValidateCheckResults(item.Definition, caseOutput.Checks); checkErr != nil {
				err = fmt.Errorf("case %s produced invalid check results: %w", caseID, checkErr)
			}
		}
		if err == nil && caseID == "installation" {
			storageClass, ok := caseOutput.Details["defaultStorageClass"].(string)
			if !ok || storageClass == "" {
				err = fmt.Errorf("installation result did not report the dynamically assigned StorageClass")
			} else {
				report.DefaultStorageClass = storageClass
			}
		}
		finished := time.Now().UTC()
		result = testkit.CaseResult{
			Status: "passed", StartedAt: started, FinishedAt: finished, Duration: finished.Sub(started),
			LogFile: logPath, OutputFile: filepath.Join(outputDir, "result.json"),
			Requirements: append([]string(nil), item.Definition.Requirements...),
		}
		if err != nil {
			result.Status = "failed"
			result.Error = err.Error()
			if errors.Is(err, context.DeadlineExceeded) {
				result.Status = "timeout"
			}
		}
		report.Cases[caseID] = result
		if err != nil {
			_ = testkit.WriteJSONAtomic(result.OutputFile, testkit.CaseResultFile{
				CaseID: caseID, Status: result.Status, StartedAt: started, Finished: finished, Error: err.Error(),
			})
		}
		if saveErr := saveRecord(stateDir, *report); saveErr != nil && err == nil {
			return caseID, saveErr
		}
		if err != nil {
			return caseID, fmt.Errorf("case %s %s: %w", caseID, result.Status, err)
		}
		outputs[caseID] = caseOutput
		fmt.Printf("PASS %s (%s)\n", caseID, result.Duration.Round(time.Millisecond))
	}
	return "", nil
}

func runCaseProcess(ctx context.Context, item testkit.Case, environment testkit.CaseContext, contextPath, logPath string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	caseContext, cancel := context.WithTimeout(ctx, time.Duration(item.Definition.TimeoutSeconds)*time.Second)
	defer cancel()
	command := exec.CommandContext(caseContext, executable, "case", "--id", item.Definition.ID, "--context-file", contextPath)
	command.Env = append(filteredEnvironment(os.Environ()),
		"TEST_RUN_ID="+environment.RunID,
		"TEST_SCENARIO="+environment.ScenarioID,
		"TEST_CASE_ID="+environment.CaseID,
		"TEST_NAMESPACE="+environment.Namespace,
		"TEST_RELEASE="+environment.Release,
		"TEST_CONTEXT_FILE="+contextPath,
		"TEST_CASE_OUTPUT_DIR="+environment.OutputDir,
	)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	command.Stdout = logFile
	command.Stderr = logFile
	err = command.Run()
	closeErr := logFile.Close()
	if err != nil {
		if errors.Is(caseContext.Err(), context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		return err
	}
	return closeErr
}

func (runtime *Runtime) cleanup(ctx context.Context, id, stateDir string, registration environment.Registration, registrationUID string, report *testkit.RunRecord) error {
	report.State = "cleaning"
	if err := saveRecord(stateDir, *report); err != nil {
		return err
	}
	registration.State = "cleaning"
	if err := runtime.Environment.UpdateRegistration(ctx, registration); err != nil {
		return err
	}
	pvcs, generated, err := runtime.Environment.OwnedResources(ctx, registration.ID, registration.Release, registration.Fullname)
	if err != nil {
		return err
	}
	report.Resources = append(report.Resources, pvcs...)
	report.Resources = append(report.Resources, generated...)
	if err := saveRecord(stateDir, *report); err != nil {
		return err
	}
	removed, err := runtime.Environment.Destroy(ctx, registration, registrationUID)
	if err != nil {
		return err
	}
	report.Resources = removed
	return nil
}

func (runtime *Runtime) startLease(parent context.Context, id, owner string) (context.Context, <-chan error, func()) {
	ctx, cancel := context.WithCancel(parent)
	errorsChannel := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewContext, renewCancel := context.WithTimeout(ctx, 10*time.Second)
				err := runtime.Environment.RenewLease(renewContext, id, owner)
				renewCancel()
				if err != nil {
					errorsChannel <- fmt.Errorf("test ID lease renewal failed: %w", err)
					cancel()
					return
				}
			}
		}
	}()
	return ctx, errorsChannel, func() {
		cancel()
		<-done
	}
}

func checkLease(ctx context.Context, leaseErrors <-chan error) error {
	select {
	case err := <-leaseErrors:
		return err
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func stateForError(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return "interrupted"
	}
	return "failed"
}
