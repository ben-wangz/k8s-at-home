package cases

import (
	"slices"
	"testing"

	"github.com/ben-wangz/registry-hub-tests/internal/environment"
	"github.com/ben-wangz/registry-hub-tests/internal/runner"
)

func TestS3PersistenceUsesFocusedFixtureAndKeepsExplicitMatrixCoverage(t *testing.T) {
	registered, err := Registry()
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := runner.New("../../../../../..", registered)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := runtime.PlanScenario("feature-backend-s3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(plan.Order, "upstream-mirror") || !slices.Contains(plan.Order, "upstream-persistence-fixture") {
		t.Fatalf("unexpected persistence dependencies: %v", plan.Order)
	}
	for _, scenario := range []string{"feature-minimal", "feature-registry-s3", "feature-upstream-matrix-s3"} {
		plan, err := runtime.PlanScenario(scenario, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(plan.Order, "upstream-mirror") {
			t.Fatalf("%s lost matrix coverage", scenario)
		}
	}
}

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

func TestLargeLayerScenariosAreExplicitAndUseTheSameCase(t *testing.T) {
	registered, err := Registry()
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := runner.New("../../../../../..", registered)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"feature-large-layer", "feature-large-layer-s3"} {
		plan, err := runtime.PlanScenario(id, nil)
		if err != nil || len(plan.Order) != 3 || plan.Order[2] != "large-layer-performance" {
			t.Fatalf("unexpected large-layer plan: %v: %v", plan.Order, err)
		}
	}
	plan, err := runtime.PlanScenario("feature-minimal", nil)
	if err != nil || slices.Contains(plan.Order, "large-layer-performance") {
		t.Fatal("large-layer measurements must remain an explicit performance scenario")
	}
}

func TestFeatureMinimalOrdersPodmanPullAfterRestartAndUpstreamCases(t *testing.T) {
	registered, err := Registry()
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := runner.New("../../../../../..", registered)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := runtime.PlanScenario("feature-minimal", nil)
	if err != nil {
		t.Fatal(err)
	}
	position := make(map[string]int, len(plan.Order))
	for index, id := range plan.Order {
		position[id] = index
	}
	for _, predecessor := range []string{"restart-reuse", "upstream-mirror", "upstream-resume"} {
		if position[predecessor] >= position["podman-upstream-pull"] {
			t.Fatalf("%s position %d must precede podman-upstream-pull position %d", predecessor, position[predecessor], position["podman-upstream-pull"])
		}
	}
}
