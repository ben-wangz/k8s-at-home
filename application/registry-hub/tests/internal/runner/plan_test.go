package runner

import (
	"reflect"
	"testing"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func TestBuildPlanExpandsAndDeduplicatesDependencies(t *testing.T) {
	cases := map[string]testkit.Case{
		"base":  {Definition: testkit.Definition{ID: "base"}},
		"left":  {Definition: testkit.Definition{ID: "left", DependsOn: []string{"base"}}},
		"right": {Definition: testkit.Definition{ID: "right", DependsOn: []string{"base"}}},
	}

	plan, err := BuildPlan(cases, []string{"right", "left", "right"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Explicit, []string{"left", "right"}) {
		t.Fatalf("explicit selection = %v", plan.Explicit)
	}
	if !reflect.DeepEqual(plan.Added, []string{"base"}) {
		t.Fatalf("added dependencies = %v", plan.Added)
	}
	if !reflect.DeepEqual(plan.Order, []string{"base", "left", "right"}) {
		t.Fatalf("execution order = %v", plan.Order)
	}
}

func TestBuildPlanOrdersRunAfterOnlyWhenBothCasesAreSelected(t *testing.T) {
	cases := map[string]testkit.Case{
		"installation": {Definition: testkit.Definition{ID: "installation"}},
		"upstream":     {Definition: testkit.Definition{ID: "upstream"}},
		"podman": {Definition: testkit.Definition{
			ID:        "podman",
			DependsOn: []string{"installation"},
			RunAfter:  []string{"upstream"},
		}},
	}

	selected, err := BuildPlan(cases, []string{"podman"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(selected.Order, []string{"installation", "podman"}) {
		t.Fatalf("selected plan order = %v", selected.Order)
	}
	if !reflect.DeepEqual(selected.Added, []string{"installation"}) {
		t.Fatalf("selected plan dependencies = %v", selected.Added)
	}

	full, err := BuildPlan(cases, []string{"podman", "upstream"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(full.Order, []string{"installation", "upstream", "podman"}) {
		t.Fatalf("full plan order = %v", full.Order)
	}
}

func TestBuildPlanRejectsMissingDependency(t *testing.T) {
	cases := map[string]testkit.Case{
		"consumer": {Definition: testkit.Definition{ID: "consumer", DependsOn: []string{"missing"}}},
	}

	if _, err := BuildPlan(cases, []string{"consumer"}); err == nil {
		t.Fatal("expected missing dependency error")
	}
}

func TestBuildPlanReportsDependencyCycle(t *testing.T) {
	cases := map[string]testkit.Case{
		"first":  {Definition: testkit.Definition{ID: "first", DependsOn: []string{"second"}}},
		"second": {Definition: testkit.Definition{ID: "second", DependsOn: []string{"first"}}},
	}

	if _, err := BuildPlan(cases, []string{"first"}); err == nil {
		t.Fatal("expected dependency cycle error")
	}
}

func TestBuildPlanReportsRunAfterCycle(t *testing.T) {
	cases := map[string]testkit.Case{
		"first":  {Definition: testkit.Definition{ID: "first", RunAfter: []string{"second"}}},
		"second": {Definition: testkit.Definition{ID: "second", RunAfter: []string{"first"}}},
	}

	if _, err := BuildPlan(cases, []string{"first", "second"}); err == nil {
		t.Fatal("expected runAfter cycle error")
	}
}
