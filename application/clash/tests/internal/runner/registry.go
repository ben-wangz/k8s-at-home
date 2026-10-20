package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/ben-wangz/clash-tests/features/minimal/cases"
	"github.com/ben-wangz/clash-tests/internal/testkit"
)

type Scenario struct {
	ID        string   `json:"id"`
	Directory string   `json:"directory"`
	Cases     []string `json:"cases"`
}

func Registry(app string) ([]testkit.Definition, map[string]Scenario, error) {
	definitions, err := cases.Definitions()
	if err != nil {
		return nil, nil, err
	}
	if len(definitions) == 0 {
		return nil, nil, fmt.Errorf("no Clash cases registered")
	}
	if _, err := Plan(definitions, nil); err != nil {
		return nil, nil, err
	}
	var records []Scenario
	if err := testkit.ReadJSON(filepath.Join(app, "tests/features/scenarios.json"), &records); err != nil {
		return nil, nil, err
	}
	scenarios := make(map[string]Scenario)
	for _, record := range records {
		if _, exists := scenarios[record.ID]; exists {
			return nil, nil, fmt.Errorf("duplicate scenario %s", record.ID)
		}
		if _, err := os.Stat(filepath.Join(app, "tests/features", record.Directory, "config.yaml")); err != nil {
			return nil, nil, err
		}
		if _, err := Plan(definitions, record.Cases); err != nil {
			return nil, nil, err
		}
		scenarios[record.ID] = record
	}
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].ID < definitions[j].ID })
	return definitions, scenarios, nil
}

func ScenarioPlan(definitions []testkit.Definition, scenario Scenario, selected []string) ([]testkit.Definition, error) {
	allowed, err := Plan(definitions, scenario.Cases)
	if err != nil {
		return nil, err
	}
	if len(selected) == 0 {
		return allowed, nil
	}
	return Plan(allowed, selected)
}
