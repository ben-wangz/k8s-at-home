package environment

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRegistrationExpiryHonorsRecentActivityAndIntentionalEnvironments(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	for _, test := range []struct {
		name     string
		state    string
		started  time.Time
		activity time.Time
		lease    time.Time
		want     bool
	}{
		{name: "failed", state: "failed", started: old, want: true},
		{name: "interrupted", state: "interrupted", started: old, want: true},
		{name: "cleanup failed", state: "cleanup_failed", started: old, want: true},
		{name: "abandoned", state: "running", started: old, lease: old.Add(time.Minute), want: true},
		{name: "explicit environment", state: "environment-ready", started: old},
		{name: "active", state: "running", started: old, lease: now.Add(time.Minute)},
		{name: "resumed", state: "failed", started: old, activity: now.Add(-time.Hour)},
		{name: "legacy recent lease", state: "failed", started: old, lease: now.Add(-time.Minute)},
		{name: "future", state: "failed", started: now.Add(time.Hour)},
		{name: "missing timestamps", state: "failed"},
		{name: "unknown state", state: "custom", started: old},
	} {
		t.Run(test.name, func(t *testing.T) {
			registration := Registration{State: test.state, StartedAt: test.started, LastActivityAt: test.activity, LeaseUntil: test.lease}
			if got := RegistrationExpired(registration, now, 24*time.Hour); got != test.want {
				t.Fatalf("expired=%v want=%v", got, test.want)
			}
		})
	}
}

func TestExpiredClaimRechecksActivityBeforeConditionalReplacement(t *testing.T) {
	for _, recent := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired", true: "renewed since listing"}[recent], func(t *testing.T) {
			directory := t.TempDir()
			registration := Registration{ID: "expired", ScenarioID: "feature-minimal", Release: "rh-min-expired", Fullname: "rh-min-expired-registry-hub", KubeContext: "test", State: "failed", StartedAt: time.Now().Add(-48 * time.Hour)}
			if recent {
				registration.LastActivityAt = time.Now()
			}
			object := map[string]any{"metadata": map[string]any{"uid": "identity", "resourceVersion": "42", "labels": map[string]string{"registry-hub.io/test-id": registration.ID, "registry-hub.io/managed-by": runnerManagedBy}}, "data": registrationData(registration)}
			encoded, err := json.Marshal(object)
			if err != nil {
				t.Fatal(err)
			}
			writeLeaseFixture(t, filepath.Join(directory, "object.json"), encoded, 0600)
			writeLeaseFixture(t, filepath.Join(directory, "kubectl"), []byte(leaseKubectlFixture), 0700)
			t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("LEASE_FIXTURE", directory)
			claimed, _, err := (Environment{}).ClaimExpiredRegistration(context.Background(), registration.ID, "pruner", "test", 24*time.Hour)
			if recent {
				if !errors.Is(err, ErrRegistrationNotExpired) {
					t.Fatalf("renewed run was claimed: %v", err)
				}
				if _, err := os.Stat(filepath.Join(directory, "replacement.json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("renewed registration was mutated: %v", err)
				}
				return
			}
			if err != nil || claimed.ActiveOwner != "pruner" || !claimed.LeaseUntil.After(time.Now()) {
				t.Fatalf("claim=%+v err=%v", claimed, err)
			}
			data, err := os.ReadFile(filepath.Join(directory, "replacement.json"))
			if err != nil || json.Unmarshal(data, &object) != nil || testResourceVersion(object) != "42" {
				t.Fatalf("claim dropped its resourceVersion precondition: %s %v", data, err)
			}
		})
	}
}

func testResourceVersion(object map[string]any) any {
	return object["metadata"].(map[string]any)["resourceVersion"]
}
