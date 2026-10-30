package environment

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenewLeaseUsesOneConditionalReadAndWrite(t *testing.T) {
	for _, test := range []struct {
		name     string
		owner    string
		expired  bool
		conflict bool
		calls    int
	}{
		{name: "owned", owner: "runner", calls: 2},
		{name: "conflict", owner: "runner", conflict: true, calls: 4},
		{name: "other_owner", owner: "other", calls: 1},
		{name: "expired", owner: "runner", expired: true, calls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			registration := Registration{ID: "renewal", ScenarioID: "feature-minimal", Release: "rh-min-renewal", Fullname: "rh-min-renewal-registry-hub", KubeContext: "test", State: "running", ActiveOwner: test.owner, LeaseUntil: time.Now().Add(time.Minute)}
			if test.expired {
				registration.LeaseUntil = time.Now().Add(-time.Second)
			}
			object := map[string]any{"metadata": map[string]any{"uid": "identity", "resourceVersion": "42", "labels": map[string]string{"registry-hub.io/test-id": registration.ID, "registry-hub.io/managed-by": runnerManagedBy}}, "data": registrationData(registration)}
			encoded, err := json.Marshal(object)
			if err != nil {
				t.Fatal(err)
			}
			writeLeaseFixture(t, filepath.Join(directory, "object.json"), encoded, 0600)
			writeLeaseFixture(t, filepath.Join(directory, "kubectl"), []byte(leaseKubectlFixture), 0700)
			if test.conflict {
				writeLeaseFixture(t, filepath.Join(directory, "conflict"), nil, 0600)
			}
			t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("LEASE_FIXTURE", directory)
			err = (Environment{}).RenewLease(context.Background(), registration.ID, "runner")
			if (err != nil) != (test.calls == 1) {
				t.Fatalf("renewal error=%v", err)
			}
			commands, err := os.ReadFile(filepath.Join(directory, "calls"))
			if err != nil || len(strings.Fields(string(commands))) != test.calls {
				t.Fatalf("commands=%q error=%v", commands, err)
			}
			if test.calls == 1 {
				return
			}
			replacement, err := os.ReadFile(filepath.Join(directory, "replacement.json"))
			if err != nil || json.Unmarshal(replacement, &object) != nil {
				t.Fatalf("invalid replacement: %v", err)
			}
			updated, identity, err := parseRegistration(object, registration.ID)
			if err != nil || identity.UID != "identity" || object["metadata"].(map[string]any)["resourceVersion"] != "42" || updated.State != "running" || updated.ActiveOwner != "runner" || !updated.LeaseUntil.After(time.Now().Add(RunnerLeaseDuration-time.Second)) {
				t.Fatalf("renewal changed ownership, lost CAS or failed to extend the lease: %+v error=%v", updated, err)
			}
		})
	}
}

func writeLeaseFixture(t *testing.T, name string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(name, data, mode); err != nil {
		t.Fatal(err)
	}
}

const leaseKubectlFixture = `#!/bin/sh
if [ "$3" = "get" ]; then
  /bin/echo get >> "$LEASE_FIXTURE/calls"
  /bin/cat "$LEASE_FIXTURE/object.json"
  exit 0
fi
/bin/echo replace >> "$LEASE_FIXTURE/calls"
/bin/cat > "$LEASE_FIXTURE/replacement.json"
if [ -f "$LEASE_FIXTURE/conflict" ]; then
  /bin/rm "$LEASE_FIXTURE/conflict"
  /bin/echo 'Conflict: resourceVersion changed' >&2
  exit 1
fi
`
