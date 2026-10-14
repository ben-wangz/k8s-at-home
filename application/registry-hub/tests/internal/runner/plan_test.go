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
