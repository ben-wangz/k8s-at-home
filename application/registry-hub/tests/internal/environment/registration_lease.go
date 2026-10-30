package environment

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func (environment Environment) RenewLease(ctx context.Context, id, owner string) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		object, err := testkit.KubectlGet(ctx, Namespace, "configmap", RegistrationName(id))
		if err != nil {
			return err
		}
		registration, _, err := parseRegistration(object, id)
		if err != nil {
			return err
		}
		if registration.ActiveOwner != owner || !registration.LeaseUntil.After(time.Now().UTC()) {
			return fmt.Errorf("test ID %s lease expired or was taken by another runner", id)
		}
		registration.LeaseUntil = time.Now().UTC().Add(RunnerLeaseDuration)
		object["data"] = registrationData(registration)
		encoded, err := json.Marshal(object)
		if err != nil {
			return err
		}
		_, err = testkit.CommandInput(ctx, encoded, "kubectl", "-n", Namespace, "replace", "-f", "-")
		if err == nil {
			return nil
		}
		lastErr = err
		if !strings.Contains(strings.ToLower(err.Error()), "conflict") {
			return err
		}
	}
	return fmt.Errorf("renew test ID %s after resourceVersion conflicts: %w", id, lastErr)
}
