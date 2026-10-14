package testkit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"
)

var caseIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

func LoadDefinition(files fs.FS, implementationID string) (Definition, json.RawMessage, error) {
	raw, err := fs.ReadFile(files, "case.json")
	if err != nil {
		return Definition{}, nil, fmt.Errorf("read case metadata: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var definition Definition
	if err := decoder.Decode(&definition); err != nil {
		return Definition{}, nil, fmt.Errorf("decode case metadata: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Definition{}, nil, fmt.Errorf("case metadata has trailing data")
	}
	if definition.ID != implementationID {
		return Definition{}, nil, fmt.Errorf("case metadata ID %q does not match registered ID %q", definition.ID, implementationID)
	}
	if !caseIDPattern.MatchString(definition.ID) {
		return Definition{}, nil, fmt.Errorf("case ID %q is invalid", definition.ID)
	}
	if strings.TrimSpace(definition.Description) == "" || definition.TimeoutSeconds < 1 || definition.TimeoutSeconds > 7200 ||
		!hasText(definition.Preconditions) || !hasText(definition.Assertions) || !hasText(definition.Outputs) || !hasText(definition.Requirements) {
		return Definition{}, nil, fmt.Errorf("case %q must declare its description, timeout, preconditions, assertions, outputs, and requirement coverage", definition.ID)
	}
	return definition, raw, nil
}

func hasText(values []string) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return false
		}
	}
	return true
}

func ValidateCases(cases []Case) (map[string]Case, error) {
	indexed := make(map[string]Case, len(cases))
	for _, item := range cases {
		id := item.Definition.ID
		if !caseIDPattern.MatchString(id) || item.Run == nil {
			return nil, fmt.Errorf("case %q has an invalid ID or no implementation", id)
		}
		if _, exists := indexed[id]; exists {
			return nil, fmt.Errorf("duplicate case ID %q", id)
		}
		indexed[id] = item
	}
	for id, item := range indexed {
		checkIDs := make(map[string]struct{}, len(item.Definition.Checks))
		for _, check := range item.Definition.Checks {
			if strings.TrimSpace(check.ID) == "" || strings.TrimSpace(check.Description) == "" {
				return nil, fmt.Errorf("case %q has an empty check definition", id)
			}
			if _, exists := checkIDs[check.ID]; exists {
				return nil, fmt.Errorf("case %q declares check %q more than once", id, check.ID)
			}
			checkIDs[check.ID] = struct{}{}
		}
		seen := make(map[string]struct{}, len(item.Definition.DependsOn))
		for _, dependency := range item.Definition.DependsOn {
			if dependency == id {
				return nil, fmt.Errorf("case %q depends on itself", id)
			}
			if _, exists := indexed[dependency]; !exists {
				return nil, fmt.Errorf("case %q depends on unknown case %q", id, dependency)
			}
			if _, exists := seen[dependency]; exists {
				return nil, fmt.Errorf("case %q declares dependency %q more than once", id, dependency)
			}
			seen[dependency] = struct{}{}
		}
	}
	return indexed, nil
}

func ValidateCheckResults(definition Definition, results []CheckResult) error {
	if len(definition.Checks) == 0 {
		return nil
	}
	observed := make(map[string]CheckResult, len(results))
	for _, result := range results {
		if _, exists := observed[result.ID]; exists {
			return fmt.Errorf("duplicate check result %q", result.ID)
		}
		observed[result.ID] = result
	}
	for _, check := range definition.Checks {
		result, ok := observed[check.ID]
		if !ok || result.Status != "passed" || len(result.Evidence) == 0 {
			return fmt.Errorf("check %q did not pass with evidence", check.ID)
		}
	}
	if len(observed) != len(definition.Checks) {
		return fmt.Errorf("case returned an undeclared check result")
	}
	return nil
}
