package environment

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"strings"
	"time"
)

const (
	runnerManagedBy     = "registry-hub-test-runner"
	RunnerLeaseDuration = 90 * time.Second
)

type Registration struct {
	ID                      string    `json:"id"`
	ScenarioID              string    `json:"scenarioId"`
	Release                 string    `json:"release"`
	Fullname                string    `json:"fullname"`
	KubeContext             string    `json:"kubeContext"`
	ConfigSHA256            string    `json:"configSha256"`
	ScenarioFixtureSHA256   string    `json:"scenarioFixtureSha256,omitempty"`
	ScenarioValuesSHA256    string    `json:"scenarioValuesSha256,omitempty"`
	EnvironmentValuesSHA256 string    `json:"environmentValuesSha256,omitempty"`
	SourceSHA256            string    `json:"sourceSha256"`
	ImageReference          string    `json:"imageReference,omitempty"`
	ImageDigest             string    `json:"imageDigest,omitempty"`
	ValuesFile              string    `json:"valuesFile,omitempty"`
	State                   string    `json:"state"`
	ActiveOwner             string    `json:"activeOwner,omitempty"`
	LeaseUntil              time.Time `json:"leaseUntil,omitempty"`
	StartedAt               time.Time `json:"startedAt"`
}

func RegistrationName(id string) string {
	return "registry-hub-min-" + id
}

func (environment Environment) Reserve(ctx context.Context, registration Registration) (testkit.ResourceIdentity, error) {
	payload := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      RegistrationName(registration.ID),
			"namespace": Namespace,
			"labels": map[string]string{
				"registry-hub.io/test-id":    registration.ID,
				"registry-hub.io/managed-by": runnerManagedBy,
			},
		},
		"data": registrationData(registration),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return testkit.ResourceIdentity{}, err
	}
	if _, err := testkit.CommandInput(ctx, encoded, "kubectl", "create", "-f", "-"); err != nil {
		return testkit.ResourceIdentity{}, fmt.Errorf("reserve test ID %s: %w", registration.ID, err)
	}
	object, err := testkit.KubectlGet(ctx, Namespace, "configmap", RegistrationName(registration.ID))
	if err != nil {
		return testkit.ResourceIdentity{}, err
	}
	metadata := testkit.Map(object["metadata"])
	uid := testkit.String(metadata["uid"])
	if uid == "" {
		return testkit.ResourceIdentity{}, fmt.Errorf("test ID registration %s has no UID", registration.ID)
	}
	return testkit.ResourceIdentity{Kind: "configmap", Name: RegistrationName(registration.ID), UID: uid}, nil
}

func (environment Environment) GetRegistration(ctx context.Context, id string) (Registration, testkit.ResourceIdentity, error) {
	object, err := testkit.KubectlGet(ctx, Namespace, "configmap", RegistrationName(id))
	if err != nil {
		return Registration{}, testkit.ResourceIdentity{}, err
	}
	return parseRegistration(object, id)
}

func parseRegistration(object map[string]any, id string) (Registration, testkit.ResourceIdentity, error) {
	metadata := testkit.Map(object["metadata"])
	labels := testkit.Map(metadata["labels"])
	if testkit.String(labels["registry-hub.io/test-id"]) != id || testkit.String(labels["registry-hub.io/managed-by"]) != runnerManagedBy {
		return Registration{}, testkit.ResourceIdentity{}, fmt.Errorf("test ID registration %s has unexpected ownership labels", id)
	}
	data := testkit.Map(object["data"])
	registration := Registration{
		ID:                      testkit.String(data["id"]),
		ScenarioID:              testkit.String(data["scenarioId"]),
		Release:                 testkit.String(data["release"]),
		Fullname:                testkit.String(data["fullname"]),
		KubeContext:             testkit.String(data["kubeContext"]),
		ConfigSHA256:            testkit.String(data["configSha256"]),
		ScenarioFixtureSHA256:   testkit.String(data["scenarioFixtureSha256"]),
		ScenarioValuesSHA256:    testkit.String(data["scenarioValuesSha256"]),
		EnvironmentValuesSHA256: testkit.String(data["environmentValuesSha256"]),
		SourceSHA256:            testkit.String(data["sourceSha256"]),
		ImageReference:          testkit.String(data["imageReference"]),
		ImageDigest:             testkit.String(data["imageDigest"]),
		ValuesFile:              testkit.String(data["valuesFile"]),
		State:                   testkit.String(data["state"]),
		ActiveOwner:             testkit.String(data["activeOwner"]),
	}
	if started := testkit.String(data["startedAt"]); started != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, started)
		if parseErr != nil {
			return Registration{}, testkit.ResourceIdentity{}, fmt.Errorf("test ID registration has an invalid timestamp")
		}
		registration.StartedAt = parsed
	}
	if leaseUntil := testkit.String(data["leaseUntil"]); leaseUntil != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, leaseUntil)
		if parseErr != nil {
			return Registration{}, testkit.ResourceIdentity{}, fmt.Errorf("test ID registration has an invalid lease time")
		}
		registration.LeaseUntil = parsed
	}
	if registration.ID != id || registration.ScenarioID == "" || registration.Release == "" || registration.Fullname == "" || registration.KubeContext == "" {
		return Registration{}, testkit.ResourceIdentity{}, fmt.Errorf("test ID registration is incomplete")
	}
	return registration, testkit.ResourceIdentity{Kind: "configmap", Name: RegistrationName(id), UID: testkit.String(metadata["uid"])}, nil
}

func (environment Environment) UpdateRegistration(ctx context.Context, registration Registration) error {
	return environment.replaceRegistration(ctx, registration, registration.ActiveOwner, false)
}

func (environment Environment) replaceRegistration(ctx context.Context, registration Registration, expectedOwner string, releaseLease bool) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		object, err := testkit.KubectlGet(ctx, Namespace, "configmap", RegistrationName(registration.ID))
		if err != nil {
			return err
		}
		current, _, err := parseRegistration(object, registration.ID)
		if err != nil {
			return err
		}
		if current.ActiveOwner != expectedOwner {
			return fmt.Errorf("test ID %s lease is no longer owned by this runner", registration.ID)
		}
		candidate := registration
		if releaseLease {
			candidate.ActiveOwner = ""
			candidate.LeaseUntil = time.Time{}
		} else if current.LeaseUntil.After(candidate.LeaseUntil) {
			candidate.LeaseUntil = current.LeaseUntil
		}
		object["data"] = registrationData(candidate)
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
	return fmt.Errorf("update test ID %s after resourceVersion conflicts: %w", registration.ID, lastErr)
}

func (environment Environment) ClaimRegistration(ctx context.Context, id, owner, contextName string) (Registration, testkit.ResourceIdentity, error) {
	object, err := testkit.KubectlGet(ctx, Namespace, "configmap", RegistrationName(id))
	if err != nil {
		return Registration{}, testkit.ResourceIdentity{}, err
	}
	registration, identity, err := parseRegistration(object, id)
	if err != nil {
		return Registration{}, testkit.ResourceIdentity{}, err
	}
	if registration.KubeContext != contextName {
		return Registration{}, testkit.ResourceIdentity{}, fmt.Errorf("test ID %s belongs to Kubernetes context %s", id, registration.KubeContext)
	}
	if registration.ActiveOwner != "" && registration.LeaseUntil.After(time.Now().UTC()) {
		return Registration{}, testkit.ResourceIdentity{}, fmt.Errorf("test ID %s is active on another runner until %s", id, registration.LeaseUntil.UTC().Format(time.RFC3339))
	}
	registration.ActiveOwner = owner
	registration.LeaseUntil = time.Now().UTC().Add(RunnerLeaseDuration)
	metadata := testkit.Map(object["metadata"])
	object["data"] = registrationData(registration)
	encoded, err := json.Marshal(object)
	if err != nil {
		return Registration{}, testkit.ResourceIdentity{}, err
	}
	if _, err := testkit.CommandInput(ctx, encoded, "kubectl", "-n", Namespace, "replace", "-f", "-"); err != nil {
		return Registration{}, testkit.ResourceIdentity{}, fmt.Errorf("claim test ID %s: %w", id, err)
	}
	identity.UID = testkit.String(metadata["uid"])
	return registration, identity, nil
}

func (environment Environment) ReleaseLease(ctx context.Context, id, owner string) error {
	registration, _, err := environment.GetRegistration(ctx, id)
	if err != nil {
		return err
	}
	if registration.ActiveOwner == "" {
		return nil
	}
	if registration.ActiveOwner != owner {
		return fmt.Errorf("test ID %s lease is owned by a different runner", id)
	}
	return environment.replaceRegistration(ctx, registration, owner, true)
}

func (environment Environment) DeleteRegistration(ctx context.Context, id, expectedUID string) error {
	registration, identity, err := environment.GetRegistration(ctx, id)
	if err != nil {
		if testkit.IsNotFound(err) {
			return nil
		}
		return err
	}
	if registration.ID != id || expectedUID == "" || identity.UID != expectedUID {
		return fmt.Errorf("test ID registration %s UID or identity changed; refusing deletion", id)
	}
	_, err = testkit.Command(ctx, "kubectl", "-n", Namespace, "delete", "configmap", identity.Name, "--wait=true", "--timeout=60s")
	return err
}
