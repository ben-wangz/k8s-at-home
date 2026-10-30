package cases

import (
	"testing"

	"github.com/ben-wangz/registry-hub-tests/internal/environment"
	"github.com/ben-wangz/registry-hub-tests/internal/runner"
)

func TestEveryRegisteredScenarioHasAValidPlanAndReleaseIdentity(t *testing.T) {
	registered, err := Registry()
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := runner.New("../../../../../..", registered)
	if err != nil {
		t.Fatal(err)
	}
	if len(runtime.Scenarios) != len(environment.ScenarioIDs()) {
		t.Fatal("runner and environment scenario registries differ")
	}
	for id := range runtime.Scenarios {
		if _, err := environment.ReleaseName(id, "identity-test"); err != nil {
			t.Fatal(err)
		}
		plan, err := runtime.PlanScenario(id, nil)
		if err != nil || len(plan.Order) == 0 {
			t.Fatalf("scenario %s has no valid plan: %v", id, err)
		}
	}
}
