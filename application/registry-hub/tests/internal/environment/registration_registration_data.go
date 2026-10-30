package environment

import (
	"fmt"
	"sort"
	"time"
)

func registrationData(registration Registration) map[string]string {
	return map[string]string{
		"id":                      registration.ID,
		"scenarioId":              registration.ScenarioID,
		"release":                 registration.Release,
		"fullname":                registration.Fullname,
		"kubeContext":             registration.KubeContext,
		"configSha256":            registration.ConfigSHA256,
		"scenarioFixtureSha256":   registration.ScenarioFixtureSHA256,
		"scenarioValuesSha256":    registration.ScenarioValuesSHA256,
		"environmentValuesSha256": registration.EnvironmentValuesSHA256,
		"sourceSha256":            registration.SourceSHA256,
		"imageReference":          registration.ImageReference,
		"imageDigest":             registration.ImageDigest,
		"valuesFile":              registration.ValuesFile,
		"state":                   registration.State,
		"startedAt":               registration.StartedAt.UTC().Format(time.RFC3339Nano),
		"activeOwner":             registration.ActiveOwner,
		"leaseUntil":              formatTimestamp(registration.LeaseUntil),
	}
}

func formatTimestamp(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func ValidateRegistration(registration Registration, id, contextName string) error {
	if registration.ID != id || registration.KubeContext != contextName {
		return fmt.Errorf("test ID %s belongs to a different identity or Kubernetes context", id)
	}
	expected, err := ReleaseName(registration.ScenarioID, id)
	if err != nil || registration.Release != expected || registration.Fullname != registration.Release+"-registry-hub" {
		return fmt.Errorf("test ID %s has unexpected release ownership", id)
	}
	return nil
}

var scenarioReleaseKeys = map[string]string{
	"feature-minimal":                 "min",
	"feature-client-auth-catalog":     "authcat",
	"feature-upstream-auth-transport": "upauth",
	"feature-backend-s3":              "backs3",
	"feature-diagnostics":             "diag",
	"feature-diagnostics-s3":          "diags3",
	"feature-registry-s3":             "regs3",
	"feature-storage-redirect":        "strdr",
	"feature-referrers-fallback":      "reffb",
	"feature-gc":                      "gc",
}

func ScenarioIDs() []string {
	ids := make([]string, 0, len(scenarioReleaseKeys))
	for id := range scenarioReleaseKeys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func ReleaseName(scenarioID, id string) (string, error) {
	key, exists := scenarioReleaseKeys[scenarioID]
	if !exists {
		return "", fmt.Errorf("unknown test scenario %q", scenarioID)
	}
	return "rh-" + key + "-" + id, nil
}
