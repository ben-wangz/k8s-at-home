package gc

import (
	"strings"
	"testing"
)

func TestRequireNoProtectionBlockersIncludesBoundedSessionState(t *testing.T) {
	status := map[string]any{
		"gate": map[string]any{"state": "open"},
		"sessions": []any{map[string]any{
			"id": "fd7af163bf5012e0eb09ab0f44811dfa", "scopeID": "root:sha256:fixture",
			"state": "protected", "ownerCount": 2, "pendingOperations": 0,
			"cancelPhase": "stop_requested", "recoveryReason": "terminal_cleanup",
		}},
	}
	err := requireNoProtectionBlockers(status)
	if err == nil {
		t.Fatal("requireNoProtectionBlockers() returned nil for a protected session")
	}
	for _, expected := range []string{"fd7af163bf5012e0eb09ab0f44811dfa", `"ownerCount":2`, `"cancelPhase":"stop_requested"`, `"recoveryReason":"terminal_cleanup"`} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("blocker error %q does not include %q", err, expected)
		}
	}
}
