package environment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

var ErrRegistrationNotExpired = errors.New("test registration is no longer expired")

func RegistrationExpired(registration Registration, now time.Time, olderThan time.Duration) bool {
	if olderThan <= 0 || registration.LeaseUntil.After(now) {
		return false
	}
	switch registration.State {
	case "failed", "interrupted", "cleanup_failed", "preparing", "deploying", "running", "cleaning":
	default:
		return false
	}
	activity := registration.StartedAt
	if registration.LastActivityAt.After(activity) {
		activity = registration.LastActivityAt
	}
	if registration.LeaseUntil.After(activity) {
		activity = registration.LeaseUntil
	}
	return !activity.IsZero() && !activity.After(now.Add(-olderThan))
}

func (environment Environment) Registrations(ctx context.Context) ([]Registration, error) {
	objects, err := testkit.KubectlJSON(ctx, Namespace, "get", "configmaps", "-l", "registry-hub.io/managed-by="+runnerManagedBy)
	if err != nil {
		return nil, err
	}
	registrations := []Registration{}
	for _, raw := range testkit.Slice(objects["items"]) {
		object := testkit.Map(raw)
		metadata := testkit.Map(object["metadata"])
		id := testkit.String(testkit.Map(metadata["labels"])["registry-hub.io/test-id"])
		if err := testkit.ValidateRunID(id); err != nil {
			return nil, err
		}
		registration, identity, err := parseRegistration(object, id)
		if err != nil {
			return nil, err
		}
		if testkit.String(metadata["name"]) != RegistrationName(id) || identity.UID == "" {
			return nil, fmt.Errorf("test registration %s has an unexpected object identity", id)
		}
		registrations = append(registrations, registration)
	}
	return registrations, nil
}

func (environment Environment) ClaimExpiredRegistration(ctx context.Context, id, owner, contextName string, olderThan time.Duration) (Registration, testkit.ResourceIdentity, error) {
	if olderThan <= 0 {
		return Registration{}, testkit.ResourceIdentity{}, fmt.Errorf("retention duration must be positive")
	}
	return environment.claimRegistration(ctx, id, owner, contextName, olderThan)
}
