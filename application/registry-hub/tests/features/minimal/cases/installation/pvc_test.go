package installation

import "testing"

func TestPersistentClaimsFollowEffectiveStorageConfiguration(t *testing.T) {
	for _, backend := range []string{"filesystem", "s3"} {
		for _, registry := range []string{"filesystem", "s3"} {
			values := map[string]any{"storage": map[string]any{"backend": backend}, "registry": map[string]any{"storage": map[string]any{"type": registry}}}
			expected, err := expectedPersistentClaims(values, "test")
			if err != nil {
				t.Fatal(err)
			}
			if (expected["test-backend"] == "20Gi") != (backend == "filesystem") || (expected["test-registry"] == "100Gi") != (registry == "filesystem") {
				t.Fatalf("incorrect claims for %s/%s: %v", backend, registry, expected)
			}
		}
	}
	if _, err := expectedPersistentClaims(nil, "test"); err == nil {
		t.Fatal("unknown storage configuration accepted")
	}
}
