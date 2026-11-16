package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/environment"
)

const FailedEnvironmentRetention = 24 * time.Hour

func (runtime *Runtime) Prune(ctx context.Context, olderThan time.Duration, dryRun bool, excludedID string) error {
	if olderThan <= 0 {
		return fmt.Errorf("--older-than must be positive")
	}
	contextName, err := runtime.Environment.CurrentContext(ctx)
	if err != nil {
		return err
	}
	registrations, err := runtime.Environment.Registrations(ctx)
	if err != nil {
		return err
	}
	for _, registration := range registrations {
		if registration.ID == excludedID || registration.KubeContext != contextName {
			continue
		}
		if err := environment.ValidateRegistration(registration, registration.ID, contextName); err != nil {
			return err
		}
		if !environment.RegistrationExpired(registration, time.Now().UTC(), olderThan) {
			continue
		}
		if dryRun {
			fmt.Printf("Expired test environment: %s (%s, %s)\n", registration.ID, registration.ScenarioID, registration.State)
			continue
		}
		if err := runtime.destroy(ctx, registration.ID, olderThan); err != nil && !errors.Is(err, environment.ErrRegistrationNotExpired) {
			return fmt.Errorf("prune test environment %s: %w", registration.ID, err)
		}
	}
	return nil
}
