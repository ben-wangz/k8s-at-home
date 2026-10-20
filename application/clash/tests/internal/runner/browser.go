package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ben-wangz/clash-tests/internal/testkit"
)

type browserReport struct {
	Suites []browserSuite `json:"suites"`
}
type browserSuite struct {
	Suites []browserSuite `json:"suites"`
	Specs  []struct {
		Title string `json:"title"`
		Tests []struct {
			Results []struct {
				Status      string `json:"status"`
				Attachments []struct {
					Path string `json:"path"`
					Body string `json:"body"`
				} `json:"attachments"`
			} `json:"results"`
		} `json:"tests"`
	} `json:"specs"`
}

func browserChecks(report browserReport, evidence string, definition testkit.Definition, mode string) []testkit.Result {
	var results []testkit.Result
	checks := make(map[string]testkit.Check, len(definition.Checks))
	for _, check := range definition.Checks {
		checks[check.ID] = check
	}
	var visit func(browserSuite)
	visit = func(suite browserSuite) {
		for _, child := range suite.Suites {
			visit(child)
		}
		for _, spec := range suite.Specs {
			result := testkit.Result{ID: spec.Title, Status: "failed"}
			for _, test := range spec.Tests {
				if len(test.Results) == 0 {
					continue
				}
				last := test.Results[len(test.Results)-1]
				if last.Status == "passed" && len(last.Attachments) > 0 {
					result.Status = "passed"
					for _, expected := range checks[spec.Title].ExpectedEvidence {
						if len(expected.Modes) == 0 || contains(expected.Modes, mode) {
							result.Evidence = append(result.Evidence, testkit.Evidence{ID: expected.ID, Path: evidence})
						}
					}
				}
			}
			results = append(results, result)
		}
	}
	for _, suite := range report.Suites {
		visit(suite)
	}
	return results
}

func runBrowser(ctx context.Context, app, directory string, definition testkit.Definition, mode string) ([]testkit.Result, error) {
	caseDirectory := filepath.Join(directory, "cases", definition.ID)
	if err := os.MkdirAll(caseDirectory, 0700); err != nil {
		return nil, err
	}
	reportPath := filepath.Join(caseDirectory, "browser.json")
	contextPath := filepath.Join(directory, "browser-context.json")
	if definition.Mode == "mock-browser" {
		contextPath = filepath.Join(caseDirectory, "mock-context.json")
		if err := testkit.WriteJSON(contextPath, map[string]string{"mode": "mock"}); err != nil {
			return nil, err
		}
	}
	variables := []string{"CLASH_TEST_RUN_CONTEXT=" + contextPath, "CLASH_TEST_OUTPUT_DIR=" + filepath.Join(caseDirectory, "artifacts"), "PLAYWRIGHT_JSON_OUTPUT_NAME=" + reportPath}
	output, commandErr := testkit.Command(ctx, filepath.Join(app, "tests/browser"), variables, "npm", "exec", "--", "playwright", "test", definition.Spec, "--grep", definition.ID+`\.`, "--reporter=json")
	_ = os.WriteFile(filepath.Join(caseDirectory, "browser.log"), output, 0600)
	var report browserReport
	if err := testkit.ReadJSON(reportPath, &report); err != nil {
		return nil, fmt.Errorf("browser report unavailable; see %s", filepath.Join(caseDirectory, "browser.log"))
	}
	checks := browserChecks(report, reportPath, definition, mode)
	if commandErr != nil {
		return checks, fmt.Errorf("browser assertions failed; see %s", reportPath)
	}
	return checks, nil
}
