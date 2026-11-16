package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/environment"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func (runtime *Runtime) preserveRunFailure(cause error, state, stateDir string, registration environment.Registration, registrationUID string, report *testkit.RunRecord) error {
	report.State = state
	report.Failure = cause.Error()
	now := time.Now().UTC()
	report.FinishedAt = &now
	report.WorkloadsPaused = false
	report.PauseError = ""
	if state == "cleanup_failed" {
		report.CleanupError = cause.Error()
		fmt.Fprintf(os.Stderr, "Cases passed; cleanup failed. Inspect or destroy test ID %s.\n", registration.ID)
	}
	// Finalization must run after signal cancellation and finish before its renewed lease expires.
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	pauseErr := runtime.Environment.RenewLease(ctx, registration.ID, registration.ActiveOwner)
	if pauseErr == nil {
		pauseErr = runtime.Environment.Pause(ctx, registration, registrationUID)
	}
	if pauseErr != nil {
		report.PauseError = pauseErr.Error()
		fmt.Fprintf(os.Stderr, "Could not pause test environment %s: %v\n", registration.ID, pauseErr)
	} else {
		report.WorkloadsPaused = true
		fmt.Fprintf(os.Stderr, "Test environment %s paused; PVCs and reports retained for reuse or destruction.\n", registration.ID)
	}
	registration.State = state
	updateContext, updateCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer updateCancel()
	updateErr := runtime.Environment.UpdateRegistration(updateContext, registration)
	saveErr := saveRecord(stateDir, *report)
	printSummary(*report)
	return errors.Join(cause, pauseErr, updateErr, saveErr)
}
