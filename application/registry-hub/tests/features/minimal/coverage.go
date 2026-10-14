package minimal

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

//go:embed coverage.json
var coverageData []byte

type coverageEntry struct {
	ID             string   `json:"id"`
	Classification string   `json:"classification"`
	CaseIDs        []string `json:"caseIds"`
	TestIDs        []string `json:"testIds"`
}

type coverageDocument struct {
	Entries          []coverageEntry    `json:"entries"`
	FeatureScenarios []scenarioCoverage `json:"featureScenarios"`
}

type scenarioCoverage struct {
	ID           string   `json:"id"`
	ConfigPath   string   `json:"configPath"`
	CaseIDs      []string `json:"caseIds"`
	Requirements []string `json:"requirements"`
}

var coverageIDPattern = regexp.MustCompile(`^A[1-8]\.[0-9]{2}$`)
var coverageTestIDPattern = regexp.MustCompile(`^(integration|unit):[a-z0-9]+(-[a-z0-9]+)*$`)
var featureScenarioPattern = regexp.MustCompile(`^feature-[a-z0-9]+(-[a-z0-9]+)*$`)
var featureConfigPathPattern = regexp.MustCompile(`^tests/features/[a-z0-9-]+/config\.yaml$`)

func ValidateCoverage(caseIDs map[string]struct{}) error {
	var document coverageDocument
	if err := json.Unmarshal(coverageData, &document); err != nil {
		return fmt.Errorf("decode minimal coverage: %w", err)
	}
	if len(document.Entries) != 111 {
		return fmt.Errorf("minimal coverage has %d entries, want 111", len(document.Entries))
	}
	seen := make(map[string]struct{}, len(document.Entries))
	for _, entry := range document.Entries {
		if !coverageIDPattern.MatchString(entry.ID) {
			return fmt.Errorf("invalid coverage ID %q", entry.ID)
		}
		if _, exists := seen[entry.ID]; exists {
			return fmt.Errorf("duplicate coverage ID %q", entry.ID)
		}
		seen[entry.ID] = struct{}{}
		switch entry.Classification {
		case "complete", "partial":
			if len(entry.CaseIDs) == 0 && len(entry.TestIDs) == 0 {
				return fmt.Errorf("coverage %s has no case or integration test", entry.ID)
			}
		case "special":
			if len(entry.CaseIDs) != 0 || len(entry.TestIDs) != 0 {
				return fmt.Errorf("special coverage %s unexpectedly names a case or integration test", entry.ID)
			}
		default:
			return fmt.Errorf("coverage %s has unknown classification %q", entry.ID, entry.Classification)
		}
		for _, caseID := range entry.CaseIDs {
			if _, exists := caseIDs[caseID]; !exists {
				return fmt.Errorf("coverage %s references unknown case %q", entry.ID, caseID)
			}
		}
		seenEntryTestIDs := make(map[string]struct{}, len(entry.TestIDs))
		for _, testID := range entry.TestIDs {
			if !coverageTestIDPattern.MatchString(testID) {
				return fmt.Errorf("coverage %s has invalid Go test ID %q", entry.ID, testID)
			}
			if _, exists := seenEntryTestIDs[testID]; exists {
				return fmt.Errorf("coverage %s repeats Go test ID %q", entry.ID, testID)
			}
			seenEntryTestIDs[testID] = struct{}{}
		}
	}
	for section, count := range [...]int{22, 16, 13, 13, 12, 15, 11, 9} {
		for item := 1; item <= count; item++ {
			id := fmt.Sprintf("A%d.%02d", section+1, item)
			if _, exists := seen[id]; !exists {
				return fmt.Errorf("minimal coverage is missing %s", id)
			}
		}
	}
	seenScenarios := make(map[string]struct{}, len(document.FeatureScenarios))
	for _, scenario := range document.FeatureScenarios {
		if !featureScenarioPattern.MatchString(scenario.ID) {
			return fmt.Errorf("invalid feature scenario ID %q", scenario.ID)
		}
		if _, exists := seenScenarios[scenario.ID]; exists {
			return fmt.Errorf("duplicate feature scenario %q", scenario.ID)
		}
		seenScenarios[scenario.ID] = struct{}{}
		expectedConfigPath := "tests/features/" + strings.TrimPrefix(scenario.ID, "feature-") + "/config.yaml"
		if !featureConfigPathPattern.MatchString(scenario.ConfigPath) || scenario.ConfigPath != expectedConfigPath {
			return fmt.Errorf("feature scenario %s has an invalid config path", scenario.ID)
		}
		if len(scenario.CaseIDs) == 0 || len(scenario.Requirements) == 0 {
			return fmt.Errorf("feature scenario %s has no cases or requirement mappings", scenario.ID)
		}
		for _, caseID := range scenario.CaseIDs {
			if _, exists := caseIDs[caseID]; !exists {
				return fmt.Errorf("feature scenario %s references unknown case %q", scenario.ID, caseID)
			}
		}
		for _, requirement := range scenario.Requirements {
			if _, exists := seen[requirement]; !exists {
				return fmt.Errorf("feature scenario %s references unknown requirement %q", scenario.ID, requirement)
			}
		}
	}
	return nil
}
