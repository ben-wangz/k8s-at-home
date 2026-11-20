package environment

import (
	"testing"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func TestGCJobIdentityUsesExactTestServiceAccount(t *testing.T) {
	job := gcJobFixture("registry-hub-gc-0123456789ab", "job-uid", "0123456789ab", "rh-gc-run-registry-hub")
	identity, owned, err := gcJobIdentity(job, "rh-gc-run-registry-hub")
	if err != nil || !owned {
		t.Fatalf("owned GC job = %v, %v; want owned without error", owned, err)
	}
	if identity != (testkit.ResourceIdentity{Kind: "job", Name: "registry-hub-gc-0123456789ab", UID: "job-uid"}) {
		t.Fatalf("GC job identity = %#v", identity)
	}

	identity, owned, err = gcJobIdentity(job, "rh-gc-other-registry-hub")
	if err != nil || owned || identity != (testkit.ResourceIdentity{}) {
		t.Fatalf("unrelated GC job = %#v, %v, %v; want no match", identity, owned, err)
	}
}

func TestGCJobIdentityRejectsMalformedOwnedJob(t *testing.T) {
	job := gcJobFixture("registry-hub-gc-0123456789ab", "job-uid", "different-id", "rh-gc-run-registry-hub")
	if _, _, err := gcJobIdentity(job, "rh-gc-run-registry-hub"); err == nil {
		t.Fatal("malformed GC identity was accepted")
	}
}

func gcJobFixture(name, uid, gcID, serviceAccount string) map[string]any {
	return map[string]any{
		"kind": "Job",
		"metadata": map[string]any{
			"name": name,
			"uid":  uid,
			"labels": map[string]any{
				"app.kubernetes.io/name":      "registry-hub",
				"app.kubernetes.io/component": "gc",
				"registry-hub.io/gc-id":       gcID,
			},
		},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{"serviceAccountName": serviceAccount},
			},
		},
	}
}
