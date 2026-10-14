package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func ExecuteCase(ctx context.Context, cases map[string]testkit.Case, id, contextPath string) error {
	item, exists := cases[id]
	if !exists {
		return fmt.Errorf("unknown case %q", id)
	}
	var environment testkit.CaseContext
	if err := testkit.ReadJSON(contextPath, &environment); err != nil {
		return err
	}
	if environment.CaseID != id || environment.Namespace != "develop" ||
		environment.RunID != os.Getenv("TEST_RUN_ID") ||
		environment.ScenarioID != os.Getenv("TEST_SCENARIO") ||
		environment.CaseID != os.Getenv("TEST_CASE_ID") ||
		environment.Namespace != os.Getenv("TEST_NAMESPACE") ||
		environment.Release != os.Getenv("TEST_RELEASE") ||
		contextPath != os.Getenv("TEST_CONTEXT_FILE") ||
		environment.OutputDir != os.Getenv("TEST_CASE_OUTPUT_DIR") {
		return fmt.Errorf("case process context does not match its run environment")
	}
	scenario, exists := scenarioRegistry()[environment.ScenarioID]
	if !exists || !containsCaseID(scenario.CaseIDs, id) {
		return fmt.Errorf("case %s is not registered for scenario %s", id, environment.ScenarioID)
	}
	started := time.Now().UTC()
	caseContext, cancel := context.WithTimeout(ctx, time.Duration(item.Definition.TimeoutSeconds)*time.Second)
	defer cancel()
	err := item.Run(caseContext, environment)
	finished := time.Now().UTC()
	if err != nil {
		result := testkit.CaseResultFile{
			CaseID: id, Status: "failed", StartedAt: started, Finished: finished, Error: err.Error(),
		}
		_ = testkit.WriteJSONAtomic(filepath.Join(environment.OutputDir, "result.json"), result)
		return err
	}
	var result testkit.CaseResultFile
	if err := testkit.ReadJSON(filepath.Join(environment.OutputDir, "result.json"), &result); err != nil {
		return fmt.Errorf("case %s returned without a structured result: %w", id, err)
	}
	if result.CaseID != id || result.Status != "passed" {
		return fmt.Errorf("case %s produced an invalid result status", id)
	}
	return nil
}

func containsCaseID(caseIDs []string, expected string) bool {
	for _, caseID := range caseIDs {
		if caseID == expected {
			return true
		}
	}
	return false
}
