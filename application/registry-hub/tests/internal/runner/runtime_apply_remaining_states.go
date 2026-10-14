package runner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func applyRemainingStates(results map[string]testkit.CaseResult, order []string, cases map[string]testkit.Case, failed string, runErr error) {
	dependents := make(map[string]struct{})
	if failed != "" {
		var addDependents func(string)
		addDependents = func(current string) {
			for id, item := range cases {
				if _, exists := dependents[id]; exists {
					continue
				}
				for _, dependency := range item.Definition.DependsOn {
					if dependency == current {
						dependents[id] = struct{}{}
						addDependents(id)
						break
					}
				}
			}
		}
		addDependents(failed)
	}
	for _, id := range order {
		if id == failed || results[id].Status != "not-run" {
			continue
		}
		result := results[id]
		if _, blocked := dependents[id]; blocked {
			result.Status = "blocked"
			result.Error = "a dependency did not pass"
		}
		results[id] = result
	}
	_ = runErr
}

func printPlan(plan Plan) {
	fmt.Printf("Selected cases: %s\n", strings.Join(plan.Explicit, ", "))
	if len(plan.Added) > 0 {
		fmt.Printf("Added dependencies: %s\n", strings.Join(plan.Added, ", "))
	}
	for _, edge := range plan.Edges {
		fmt.Printf("  %s -> %s\n", edge.Dependency, edge.Consumer)
	}
	fmt.Printf("Execution order: %s\n", strings.Join(plan.Order, " -> "))
}

func printSummary(report testkit.RunRecord) {
	fmt.Printf("Run %s: %s\n", report.ID, report.State)
	for _, id := range report.Plan {
		result := report.Cases[id]
		fmt.Printf("%s %s (%s)\n", strings.ToUpper(result.Status), id, result.Duration.Round(time.Millisecond))
	}
	root, err := testkit.StateRoot()
	if err != nil {
		fmt.Println("Report: <state-directory-unavailable>")
		return
	}
	fmt.Printf("Report: %s/run.json\n", scenarioStateDir(root, report.ScenarioID, report.ID))
}

func hashFile(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(content)
	return hex.EncodeToString(hash[:]), nil
}

func validateScenarioConfig(path string, scenario ScenarioDefinition) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if scenario.ID == "feature-minimal" && strings.TrimSpace(string(content)) != "{}" {
		return fmt.Errorf("feature-minimal config.yaml must remain an empty Helm values object")
	}
	if scenario.ClientAuthCatalog && strings.Count(string(content), "__RUN_CLIENT_AUTH_SECRET__") != 1 {
		return fmt.Errorf("feature-client-auth-catalog config must contain one client auth Secret placeholder")
	}
	if scenario.UpstreamAuth && strings.Count(string(content), "__RUN_UPSTREAM_CONFIG_SECRET__") < 1 {
		return fmt.Errorf("feature-upstream-auth-transport config must contain an upstream config Secret placeholder")
	}
	if scenario.BackendS3 {
		for _, placeholder := range []string{"__RUN_S3_ENDPOINT__", "__RUN_BACKEND_S3_BUCKET__", "__RUN_BACKEND_S3_SECRET__"} {
			if strings.Count(string(content), placeholder) != 1 {
				return fmt.Errorf("feature-backend-s3 config must contain one %s placeholder", placeholder)
			}
		}
	}
	if scenario.RegistryS3 {
		for _, placeholder := range []string{"__RUN_REGISTRY_S3_ENDPOINT__", "__RUN_REGISTRY_S3_BUCKET__", "__RUN_REGISTRY_S3_SECRET__", "__RUN_REGISTRY_S3_CA_SECRET__"} {
			if strings.Count(string(content), placeholder) != 1 {
				return fmt.Errorf("feature-registry-s3 config must contain one %s placeholder", placeholder)
			}
		}
	}
	if scenario.StorageRedirect {
		for _, placeholder := range []string{
			"__RUN_REGISTRY_S3_ENDPOINT__", "__RUN_REGISTRY_S3_BUCKET__",
			"__RUN_REGISTRY_S3_SECRET__", "__RUN_REGISTRY_S3_CA_SECRET__",
			"__RUN_STORAGE_REDIRECT_ENDPOINT__",
		} {
			if strings.Count(string(content), placeholder) != 1 {
				return fmt.Errorf("feature-storage-redirect config must contain one %s placeholder", placeholder)
			}
		}
	}
	return nil
}

func (runtime *Runtime) prepareScenario(ctx context.Context, scenario ScenarioDefinition, id, fullname, stateDir, expectedFixtureHash string, report *testkit.RunRecord) (string, string, string, error) {
	configPath := filepath.Join(runtime.Environment.AppDir, scenario.ConfigPath)
	content, err := os.ReadFile(configPath)
	if err != nil {
		return "", "", "", err
	}
	fixtureHash := ""
	if scenario.ClientAuthCatalog {
		secretName, digest, err := runtime.Environment.PrepareClientAuthSecret(ctx, id, fullname, expectedFixtureHash)
		if err != nil {
			return "", "", "", err
		}
		fixtureHash = digest
		content = []byte(strings.Replace(string(content), "__RUN_CLIENT_AUTH_SECRET__", secretName, 1))
	}
	if scenario.UpstreamAuth {
		secretName, digest, err := runtime.Environment.PrepareUpstreamConfigSecret(ctx, id, fullname, expectedFixtureHash)
		if err != nil {
			return "", "", "", err
		}
		fixtureHash = digest
		content = []byte(strings.ReplaceAll(string(content), "__RUN_UPSTREAM_CONFIG_SECRET__", secretName))
	}
	if scenario.BackendS3 {
		fixture, err := runtime.Environment.PrepareBackendS3Fixture(ctx, id, fullname, expectedFixtureHash)
		if err != nil {
			return "", "", "", err
		}
		fixtureHash = fixture.SecretSHA256
		content = []byte(strings.ReplaceAll(string(content), "__RUN_S3_ENDPOINT__", fixture.Endpoint))
		content = []byte(strings.ReplaceAll(string(content), "__RUN_BACKEND_S3_BUCKET__", fixture.Bucket))
		content = []byte(strings.ReplaceAll(string(content), "__RUN_BACKEND_S3_SECRET__", fixture.SecretName))
	}
	if scenario.RegistryS3 || scenario.StorageRedirect {
		fixture, err := runtime.Environment.PrepareRegistryS3Fixture(ctx, id, fullname, expectedFixtureHash)
		if err != nil {
			return "", "", "", err
		}
		fixtureHash = fixture.SecretSHA256
		content = []byte(strings.ReplaceAll(string(content), "__RUN_REGISTRY_S3_ENDPOINT__", fixture.Endpoint))
		content = []byte(strings.ReplaceAll(string(content), "__RUN_REGISTRY_S3_BUCKET__", fixture.Bucket))
		content = []byte(strings.ReplaceAll(string(content), "__RUN_REGISTRY_S3_SECRET__", fixture.CredentialsSecret))
		content = []byte(strings.ReplaceAll(string(content), "__RUN_REGISTRY_S3_CA_SECRET__", fixture.CASecret))
		if scenario.StorageRedirect {
			content = []byte(strings.ReplaceAll(string(content), "__RUN_STORAGE_REDIRECT_ENDPOINT__", fixture.Endpoint))
		}
	}
	if strings.Contains(string(content), "__RUN_") {
		return "", "", "", fmt.Errorf("scenario %s has an unresolved runtime placeholder", scenario.ID)
	}
	path := filepath.Join(stateDir, "scenario-values.yaml")
	if err := os.WriteFile(path, content, 0600); err != nil {
		return "", "", "", err
	}
	if err := os.Chmod(path, 0600); err != nil {
		return "", "", "", err
	}
	valuesHash, err := hashFile(path)
	if err != nil {
		return "", "", "", err
	}
	if report.ScenarioID != scenario.ID {
		return "", "", "", fmt.Errorf("scenario identity does not match the run report")
	}
	return path, fixtureHash, valuesHash, nil
}

func (runtime *Runtime) caseIndexForScenario(scenarioID string) (map[string]testkit.Case, error) {
	registered, err := runtime.CasesForScenario(scenarioID)
	if err != nil {
		return nil, err
	}
	return testkit.ValidateCases(registered)
}

func leaseOwner() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return hex.EncodeToString(random), nil
}

func filteredEnvironment(source []string) []string {
	result := make([]string, 0, len(source))
	for _, value := range source {
		if strings.HasPrefix(value, "TEST_RUN_ID=") ||
			strings.HasPrefix(value, "TEST_SCENARIO=") ||
			strings.HasPrefix(value, "TEST_CASE_ID=") ||
			strings.HasPrefix(value, "TEST_NAMESPACE=") ||
			strings.HasPrefix(value, "TEST_RELEASE=") ||
			strings.HasPrefix(value, "TEST_CONTEXT_FILE=") ||
			strings.HasPrefix(value, "TEST_CASE_OUTPUT_DIR=") {
			continue
		}
		result = append(result, value)
	}
	return result
}

func copyFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
