package installation

import (
	"net/http"
	"testing"
)

func TestExpectedClientRegistryStatus(t *testing.T) {
	deployment := func(env []any) map[string]any {
		return map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{"env": env}},
		}}}}
	}

	if got := expectedClientRegistryStatus(deployment(nil)); got != http.StatusOK {
		t.Fatalf("status without client authentication = %d, want %d", got, http.StatusOK)
	}
	authEnv := []any{map[string]any{"name": "REGISTRY_HUB_CLIENT_AUTH_CONFIG", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "client-auth"}}}}
	if got := expectedClientRegistryStatus(deployment(authEnv)); got != http.StatusUnauthorized {
		t.Fatalf("status with client authentication = %d, want %d", got, http.StatusUnauthorized)
	}
}
