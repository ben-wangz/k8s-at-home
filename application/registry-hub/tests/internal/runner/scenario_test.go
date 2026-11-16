package runner

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func TestReferrersFallbackScenarioIncludesClientInterop(t *testing.T) {
	run := func(context.Context, testkit.CaseContext) error { return nil }
	registered := map[string]testkit.Case{
		"default-render":             {Definition: testkit.Definition{ID: "default-render"}, Run: run},
		"installation":               {Definition: testkit.Definition{ID: "installation", DependsOn: []string{"default-render"}}, Run: run},
		"client-interop":             {Definition: testkit.Definition{ID: "client-interop", DependsOn: []string{"installation"}}, Run: run},
		"feature-referrers-fallback": {Definition: testkit.Definition{ID: "feature-referrers-fallback", DependsOn: []string{"installation"}}, Run: run},
	}
	runtime := &Runtime{Cases: registered, Scenarios: scenarioRegistry()}
	plan, err := runtime.PlanScenario("feature-referrers-fallback", []string{"client-interop", "feature-referrers-fallback"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"default-render", "installation", "client-interop", "feature-referrers-fallback"}
	if len(plan.Order) != len(want) {
		t.Fatalf("execution order = %v, want %v", plan.Order, want)
	}
	for index := range want {
		if plan.Order[index] != want[index] {
			t.Fatalf("execution order = %v, want %v", plan.Order, want)
		}
	}
}

func TestEmptyScenarioSelectsFeatureMinimal(t *testing.T) {
	runtime := &Runtime{Scenarios: scenarioRegistry()}
	scenario, err := runtime.scenario("")
	if err != nil {
		t.Fatal(err)
	}
	if scenario.ID != "feature-minimal" {
		t.Fatalf("default scenario = %q, want feature-minimal", scenario.ID)
	}
	if scenario.ConfigPath != filepath.Join("tests", "features", "minimal", "config.yaml") {
		t.Fatalf("default config path = %q", scenario.ConfigPath)
	}
}

func TestScenarioStateDirectoryAlwaysIncludesScenario(t *testing.T) {
	got := scenarioStateDir("/state/registry-hub", "feature-minimal", "run-1")
	want := filepath.Join("/state/registry-hub", "feature-minimal", "run-1")
	if got != want {
		t.Fatalf("state directory = %q, want %q", got, want)
	}
}

func TestPlanScenarioIgnoresRunAfterOutsideScenario(t *testing.T) {
	run := func(context.Context, testkit.CaseContext) error { return nil }
	runtime := &Runtime{
		Cases: map[string]testkit.Case{
			"first":   {Definition: testkit.Definition{ID: "first"}, Run: run},
			"later":   {Definition: testkit.Definition{ID: "later", RunAfter: []string{"outside"}}, Run: run},
			"outside": {Definition: testkit.Definition{ID: "outside"}, Run: run},
		},
		Scenarios: map[string]ScenarioDefinition{
			"feature-test": {ID: "feature-test", CaseIDs: []string{"first", "later"}},
		},
	}

	plan, err := runtime.PlanScenario("feature-test", nil)
	if err != nil {
		t.Fatalf("plan scenario: %v", err)
	}
	want := []string{"first", "later"}
	if len(plan.Order) != len(want) {
		t.Fatalf("plan order = %v, want %v", plan.Order, want)
	}
	for index := range want {
		if plan.Order[index] != want[index] {
			t.Fatalf("plan order = %v, want %v", plan.Order, want)
		}
	}
	if got := runtime.Cases["later"].Definition.RunAfter; len(got) != 1 || got[0] != "outside" {
		t.Fatalf("global run-after constraints were mutated: %v", got)
	}
}

func TestEveryScenarioConfigurationHasRequiredFixturePlaceholders(t *testing.T) {
	for id, scenario := range scenarioRegistry() {
		if err := validateScenarioConfig(filepath.Join("../../..", scenario.ConfigPath), scenario); err != nil {
			t.Fatalf("scenario %s: %v", id, err)
		}
	}
}
