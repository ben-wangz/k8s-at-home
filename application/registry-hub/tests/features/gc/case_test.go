package gc

import "testing"

func TestJobMountsVolumeTreatsMissingReadOnlyAsFalse(t *testing.T) {
	container := map[string]any{"volumeMounts": []any{
		map[string]any{"name": "registry-config", "mountPath": "/etc/distribution", "readOnly": true},
		map[string]any{"name": "registry-storage", "mountPath": "/var/lib/registry"},
	}}
	if !jobMountsVolume(container, "registry-storage", "/var/lib/registry", false) {
		t.Fatal("jobMountsVolume rejected a default writable volume mount")
	}
	if !jobMountsVolume(container, "registry-config", "/etc/distribution", true) {
		t.Fatal("jobMountsVolume rejected the read-only configuration mount")
	}
}

func TestPodClaimsMatchesOnlyTheNamedClaim(t *testing.T) {
	spec := map[string]any{"volumes": []any{
		map[string]any{"persistentVolumeClaim": map[string]any{"claimName": "other"}},
		map[string]any{"persistentVolumeClaim": map[string]any{"claimName": "registry-data"}},
	}}
	if !podClaims(spec, "registry-data") {
		t.Fatal("podClaims did not find the Registry PVC")
	}
	if podClaims(spec, "different") {
		t.Fatal("podClaims matched an unrelated PVC")
	}
}

func TestMatchesLabelsRequiresEverySelectorLabel(t *testing.T) {
	selector := map[string]any{"app": "registry", "component": "registry"}
	if !matchesLabels(map[string]any{"app": "registry", "component": "registry", "extra": "allowed"}, selector) {
		t.Fatal("matchesLabels rejected a pod with all selector labels")
	}
	if matchesLabels(map[string]any{"app": "registry"}, selector) {
		t.Fatal("matchesLabels accepted a pod missing a selector label")
	}
	if matchesLabels(map[string]any{"app": "registry"}, map[string]any{}) {
		t.Fatal("matchesLabels accepted an empty selector")
	}
}

func TestHasJobOwnerRequiresExpectedJob(t *testing.T) {
	references := []any{map[string]any{"kind": "ReplicaSet", "name": "registry-123"}, map[string]any{"kind": "Job", "name": "registry-hub-gc-abc123"}}
	if !hasJobOwner(references, "registry-hub-gc-abc123") {
		t.Fatal("hasJobOwner did not match the expected Job owner")
	}
	if hasJobOwner(references, "another-job") {
		t.Fatal("hasJobOwner matched an unrelated Job")
	}
}

func TestSameLabelsRequiresAnExactNonEmptyMatch(t *testing.T) {
	if !sameLabels(map[string]any{"app": "registry", "component": "backend"}, map[string]any{"app": "registry", "component": "backend"}) {
		t.Fatal("sameLabels rejected equal selectors")
	}
	if sameLabels(map[string]any{"app": "registry", "extra": "label"}, map[string]any{"app": "registry"}) {
		t.Fatal("sameLabels accepted a broader selector")
	}
	if sameLabels(map[string]any{}, map[string]any{}) {
		t.Fatal("sameLabels accepted empty selectors")
	}
}

func TestContainsStringMatchesPolicyType(t *testing.T) {
	if !containsString([]any{"Egress", "Ingress"}, "Ingress") {
		t.Fatal("containsString missed the ingress policy type")
	}
	if containsString([]any{"Egress"}, "Ingress") {
		t.Fatal("containsString matched an absent policy type")
	}
}
