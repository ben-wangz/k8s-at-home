package runner

import (
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/internal/environment"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"os"
	"path/filepath"
	"strings"
)

type Runtime struct {
	Environment environment.Environment
	StateRoot   string
	Cases       map[string]testkit.Case
	Scenarios   map[string]ScenarioDefinition
}

type RunOptions struct {
	ID         string
	Scenario   string
	Selected   []string
	Reuse      bool
	Image      string
	CreateOnly bool
}

func New(root string, registered []testkit.Case) (*Runtime, error) {
	env, err := environment.New(root)
	if err != nil {
		return nil, err
	}
	indexed, err := testkit.ValidateCases(registered)
	if err != nil {
		return nil, err
	}
	stateRoot, err := testkit.StateRoot()
	if err != nil {
		return nil, err
	}
	runtime := &Runtime{Environment: env, StateRoot: stateRoot, Cases: indexed}
	runtime.Scenarios = scenarioRegistry()
	for id, scenario := range runtime.Scenarios {
		if scenario.ID != id {
			return nil, fmt.Errorf("scenario registry key %q does not match scenario ID %q", id, scenario.ID)
		}
		expectedConfigPath := filepath.Join("tests", "features", strings.TrimPrefix(id, "feature-"), "config.yaml")
		if !strings.HasPrefix(id, "feature-") || scenario.ConfigPath != expectedConfigPath {
			return nil, fmt.Errorf("scenario %s must use config %s", id, expectedConfigPath)
		}
		if _, err := environment.ReleaseName(id, "check"); err != nil {
			return nil, err
		}
		if _, err := os.Stat(filepath.Join(env.AppDir, scenario.ConfigPath)); err != nil {
			return nil, fmt.Errorf("scenario %s config is unavailable: %w", id, err)
		}
		if _, err := runtime.caseIndexForScenario(id); err != nil {
			return nil, fmt.Errorf("scenario %s case registry is invalid: %w", id, err)
		}
	}
	return runtime, nil
}

func (runtime *Runtime) Plan(selected []string) (Plan, error) {
	return runtime.PlanScenario("feature-minimal", selected)
}
