package runner

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ben-wangz/clash-tests/internal/environment"
	"github.com/ben-wangz/clash-tests/internal/testkit"
)

func definition(id string, dependencies ...string) testkit.Definition {
	return testkit.Definition{ID: id, DependsOn: dependencies, TimeoutSeconds: 10, Checks: []testkit.Check{{ID: id + ".check", ExpectedEvidence: []testkit.EvidenceExpectation{{ID: "report"}}}}}
}

func TestReuseRequiresMatchingOwnedIntegration(t *testing.T) {
	base := environment.Record{ID: "run-test", Scenario: "feature-minimal", Release: "clash-test-run-test", State: "failed", Mode: "integration", SourceSHA: "source", ConfigurationSHA: "configuration", Context: "develop", Namespace: "develop"}
	if err := validateReuse(&base, "feature-minimal", "source", "configuration", "develop", "develop"); err != nil {
		t.Fatalf("matching failed integration run rejected: %v", err)
	}
	cases := map[string]func(*environment.Record){
		"release":      func(value *environment.Record) { value.Release = "another-release" },
		"scenario":     func(value *environment.Record) { value.Scenario = "feature-ingress" },
		"source":       func(value *environment.Record) { value.SourceSHA = "changed" },
		"config":       func(value *environment.Record) { value.ConfigurationSHA = "changed" },
		"context":      func(value *environment.Record) { value.Context = "another-context" },
		"namespace":    func(value *environment.Record) { value.Namespace = "another-namespace" },
		"local-mode":   func(value *environment.Record) { value.Mode = "mock" },
		"passed-state": func(value *environment.Record) { value.State = "passed" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			value := base
			mutate(&value)
			if err := validateReuse(&value, "feature-minimal", "source", "configuration", "develop", "develop"); err == nil {
				t.Fatal("invalid preserved run accepted")
			}
		})
	}
}

func TestPlanSelectionAndOrder(t *testing.T) {
	registry := []testkit.Definition{definition("selection", "ready"), definition("ready", "render"), definition("render"), definition("unselected")}
	plan, err := Plan(registry, []string{"selection", "selection"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, item := range plan {
		ids = append(ids, item.ID)
	}
	if !reflect.DeepEqual(ids, []string{"render", "ready", "selection"}) {
		t.Fatalf("unexpected dependency plan: %v", ids)
	}
	registry[0], registry[3] = registry[3], registry[0]
	other, err := Plan(registry, []string{"selection"})
	if err != nil || !reflect.DeepEqual(plan, other) {
		t.Fatalf("registry order changed the plan: %v", err)
	}
}

func TestPlanRejectsInvalidRegistries(t *testing.T) {
	for name, registry := range map[string][]testkit.Definition{
		"missing":            {definition("a", "absent")},
		"cycle":              {definition("a", "b"), definition("b", "a")},
		"duplicate":          {definition("a"), definition("a")},
		"unselected-invalid": {definition("a"), definition("broken", "absent")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Plan(registry, []string{"a"}); err == nil {
				t.Fatal("invalid registry accepted")
			}
		})
	}
	if _, err := Plan([]testkit.Definition{definition("a")}, []string{"unknown"}); err == nil {
		t.Fatal("unknown selection accepted")
	}
	incomplete := definition("without-evidence")
	incomplete.Checks[0].ExpectedEvidence = nil
	if _, err := Plan([]testkit.Definition{incomplete}, nil); err == nil {
		t.Fatal("check without expected evidence accepted")
	}
}

func TestEvidenceIsRequired(t *testing.T) {
	item := definition("selection")
	for _, results := range [][]testkit.Result{
		nil,
		{{ID: "selection.check", Status: "passed"}},
		{{ID: "selection.check", Status: "skipped", Evidence: []testkit.Evidence{{ID: "report", Path: "report"}}}},
		{{ID: "selection.check", Status: "failed", Evidence: []testkit.Evidence{{ID: "report", Path: "report"}}}},
		{{ID: "selection.check", Status: "passed", Evidence: []testkit.Evidence{{ID: "report", Path: "report"}}}, {ID: "selection.check", Status: "passed", Evidence: []testkit.Evidence{{ID: "report", Path: "report"}}}},
	} {
		if err := ValidateResults(item, results, "local"); err == nil {
			t.Fatalf("invalid evidence accepted: %+v", results)
		}
	}
	evidence := filepath.Join(t.TempDir(), "evidence.json")
	if err := os.WriteFile(evidence, []byte(`{"confirmed":"Node A"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateResults(item, []testkit.Result{{ID: "selection.check", Status: "passed", Evidence: []testkit.Evidence{{ID: "report", Path: evidence}}}}, "local"); err != nil {
		t.Fatal(err)
	}
	conditional := definition("conditional")
	conditional.Checks[0].ExpectedEvidence = append(conditional.Checks[0].ExpectedEvidence, testkit.EvidenceExpectation{ID: "integration", Modes: []string{"integration"}})
	mockResult := []testkit.Result{{ID: "conditional.check", Status: "passed", Evidence: []testkit.Evidence{{ID: "report", Path: evidence}}}}
	if err := ValidateResults(conditional, mockResult, "mock"); err != nil {
		t.Fatalf("mock evidence should not require integration-only artifacts: %v", err)
	}
	if err := ValidateResults(conditional, mockResult, "integration"); err == nil {
		t.Fatal("integration passed without its declared evidence")
	}
}

func TestBrowserSkippedAndEmptyChecksCannotPass(t *testing.T) {
	var report browserReport
	path := filepath.Join(t.TempDir(), "browser.json")
	content := `{"suites":[{"specs":[{"title":"selection.check","tests":[{"results":[{"status":"skipped","attachments":[]}]}]}]}]}`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := testkit.ReadJSON(path, &report); err != nil {
		t.Fatal(err)
	}
	if err := ValidateResults(definition("selection"), browserChecks(report, path, definition("selection"), "mock"), "mock"); err == nil {
		t.Fatal("skipped browser check passed")
	}
}
