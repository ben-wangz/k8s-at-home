package runner

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ben-wangz/clash-tests/internal/testkit"
)

func Plan(definitions []testkit.Definition, selected []string) ([]testkit.Definition, error) {
	index := make(map[string]testkit.Definition)
	checks := make(map[string]bool)
	for _, definition := range definitions {
		if definition.ID == "" || definition.TimeoutSeconds <= 0 || len(definition.Checks) == 0 {
			return nil, fmt.Errorf("incomplete case definition %q", definition.ID)
		}
		if _, exists := index[definition.ID]; exists {
			return nil, fmt.Errorf("duplicate case %s", definition.ID)
		}
		for _, check := range definition.Checks {
			if check.ID == "" || checks[check.ID] {
				return nil, fmt.Errorf("missing or duplicate check %s", check.ID)
			}
			checks[check.ID] = true
			if len(check.ExpectedEvidence) == 0 {
				return nil, fmt.Errorf("check %s has no expected evidence", check.ID)
			}
			evidenceIDs := make(map[string]bool)
			for _, evidence := range check.ExpectedEvidence {
				if evidence.ID == "" || evidenceIDs[evidence.ID] {
					return nil, fmt.Errorf("check %s has missing or duplicate expected evidence %s", check.ID, evidence.ID)
				}
				evidenceIDs[evidence.ID] = true
				for _, mode := range evidence.Modes {
					if mode != "integration" && mode != "mock" && mode != "local" {
						return nil, fmt.Errorf("check %s has unknown evidence mode %s", check.ID, mode)
					}
				}
			}
		}
		index[definition.ID] = definition
	}
	ids := make([]string, 0, len(index))
	for id := range index {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(selected) == 0 {
		selected = ids
	}
	var order []testkit.Definition
	state := make(map[string]int)
	var visit func(string, []string) error
	visit = func(id string, chain []string) error {
		definition, exists := index[id]
		if !exists {
			return fmt.Errorf("unknown case or missing dependency %s", id)
		}
		if state[id] == 1 {
			return fmt.Errorf("dependency cycle: %s", strings.Join(append(chain, id), " -> "))
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		dependencies := append([]string(nil), definition.DependsOn...)
		sort.Strings(dependencies)
		for _, dependency := range dependencies {
			if err := visit(dependency, append(chain, id)); err != nil {
				return err
			}
		}
		state[id] = 2
		order = append(order, definition)
		return nil
	}
	// Validate the entire registry before creating an environment, including unselected cases.
	for _, id := range ids {
		if err := visit(id, nil); err != nil {
			return nil, err
		}
	}
	state = make(map[string]int)
	order = nil
	selected = append([]string(nil), selected...)
	sort.Strings(selected)
	for _, id := range selected {
		if err := visit(id, nil); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func ValidateResults(definition testkit.Definition, results []testkit.Result, mode string) error {
	index := make(map[string]testkit.Result)
	for _, result := range results {
		if _, exists := index[result.ID]; exists {
			return fmt.Errorf("duplicate check result %s", result.ID)
		}
		index[result.ID] = result
	}
	for _, check := range definition.Checks {
		result, exists := index[check.ID]
		if !exists || result.Status != "passed" || len(result.Evidence) == 0 {
			return fmt.Errorf("check %s lacks successful evidence", check.ID)
		}
		expected := make(map[string]bool)
		for _, evidence := range check.ExpectedEvidence {
			if len(evidence.Modes) == 0 || contains(evidence.Modes, mode) {
				expected[evidence.ID] = true
			}
		}
		if len(expected) == 0 {
			return fmt.Errorf("check %s has no expected evidence for mode %s", check.ID, mode)
		}
		actual := make(map[string]bool)
		for _, evidence := range result.Evidence {
			if !expected[evidence.ID] {
				return fmt.Errorf("check %s returned undeclared evidence %s", check.ID, evidence.ID)
			}
			info, err := os.Stat(evidence.Path)
			if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
				return fmt.Errorf("check %s has missing or empty evidence %s", check.ID, evidence.ID)
			}
			actual[evidence.ID] = true
		}
		for id := range expected {
			if !actual[id] {
				return fmt.Errorf("check %s lacks expected evidence %s", check.ID, id)
			}
		}
	}
	return nil
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
