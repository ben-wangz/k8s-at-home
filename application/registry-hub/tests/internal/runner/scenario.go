package runner

import (
	"fmt"
	"path/filepath"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

type ScenarioDefinition struct {
	ID                string
	ConfigPath        string
	CaseIDs           []string
	ClientAuthCatalog bool
	UpstreamAuth      bool
	BackendS3         bool
	RegistryS3        bool
	StorageRedirect   bool
}

func scenarioRegistry() map[string]ScenarioDefinition {
	return map[string]ScenarioDefinition{
		"feature-minimal": {
			ID: "feature-minimal", ConfigPath: "tests/features/minimal/config.yaml",
			CaseIDs: []string{
				"admin-api", "admin-auth", "artifact-persistence", "artifact-roundtrip", "blob-mount", "blob-read",
				"blob-upload", "client-interop", "default-limits", "default-render", "delete-semantics",
				"dependency-failure", "external-combinations", "initialization-secrets", "initializer-faults",
				"installation", "manifest-validation", "observability", "rbac-boundary", "referrers-lifecycle",
				"referrers-migration", "referrers-read", "registry-protocol", "registry-roundtrip", "restart-reuse",
				"routing-paths", "security-defaults", "startup-wait", "tag-publication", "tags-pagination", "upstream-mirror",
			},
		},
		"feature-client-auth-catalog": {
			ID: "feature-client-auth-catalog", ConfigPath: "tests/features/client-auth-catalog/config.yaml",
			CaseIDs: []string{"feature-client-auth-catalog"}, ClientAuthCatalog: true,
		},
		"feature-upstream-auth-transport": {
			ID: "feature-upstream-auth-transport", ConfigPath: "tests/features/upstream-auth-transport/config.yaml",
			CaseIDs: []string{"feature-upstream-auth-transport"}, UpstreamAuth: true,
		},
		"feature-backend-s3": {
			ID: "feature-backend-s3", ConfigPath: "tests/features/backend-s3/config.yaml",
			CaseIDs: []string{"default-render", "installation", "upstream-mirror", "feature-backend-s3"}, BackendS3: true,
		},
		"feature-registry-s3": {
			ID: "feature-registry-s3", ConfigPath: "tests/features/registry-s3/config.yaml",
			CaseIDs: []string{"default-render", "installation", "upstream-mirror", "feature-registry-s3"}, RegistryS3: true,
		},
		"feature-storage-redirect": {
			ID: "feature-storage-redirect", ConfigPath: "tests/features/storage-redirect/config.yaml",
			CaseIDs: []string{"default-render", "installation", "feature-storage-redirect"}, StorageRedirect: true,
		},
		"feature-referrers-fallback": {
			ID: "feature-referrers-fallback", ConfigPath: "tests/features/referrers-fallback/config.yaml",
			CaseIDs: []string{"default-render", "installation", "client-interop", "feature-referrers-fallback"},
		},
		"feature-gc": {
			ID: "feature-gc", ConfigPath: "tests/features/gc/config.yaml",
			CaseIDs: []string{"default-render", "installation", "feature-gc"},
		},
	}
}

func (runtime *Runtime) scenario(id string) (ScenarioDefinition, error) {
	if id == "" {
		id = "feature-minimal"
	}
	scenario, exists := runtime.Scenarios[id]
	if !exists {
		return ScenarioDefinition{}, fmt.Errorf("unknown scenario %q", id)
	}
	return scenario, nil
}

func (runtime *Runtime) CasesForScenario(id string) ([]testkit.Case, error) {
	scenario, err := runtime.scenario(id)
	if err != nil {
		return nil, err
	}
	result := make([]testkit.Case, 0, len(scenario.CaseIDs))
	for _, caseID := range scenario.CaseIDs {
		item, exists := runtime.Cases[caseID]
		if !exists {
			return nil, fmt.Errorf("scenario %s references unknown case %s", scenario.ID, caseID)
		}
		result = append(result, item)
	}
	return result, nil
}

func (runtime *Runtime) PlanScenario(scenarioID string, selected []string) (Plan, error) {
	registered, err := runtime.CasesForScenario(scenarioID)
	if err != nil {
		return Plan{}, err
	}
	indexed, err := testkit.ValidateCases(registered)
	if err != nil {
		return Plan{}, err
	}
	return BuildPlan(indexed, selected)
}

func scenarioStateDir(stateRoot, scenarioID, id string) string {
	return filepath.Join(stateRoot, scenarioID, id)
}
