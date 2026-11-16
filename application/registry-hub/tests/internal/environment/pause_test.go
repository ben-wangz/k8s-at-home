package environment

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPauseGuardsOwnershipAndWaitsOnlyForDeploymentPods(t *testing.T) {
	for _, mode := range []string{"owned", "lost-owner", "expired", "changed-uid", "foreign-deployment"} {
		t.Run(mode, func(t *testing.T) {
			directory := t.TempDir()
			registration := Registration{ID: "pause", ScenarioID: "feature-minimal", Release: "rh-min-pause", Fullname: "rh-min-pause-registry-hub", KubeContext: "test", State: "running", ActiveOwner: "runner", LeaseUntil: time.Now().Add(time.Minute)}
			current := registration
			if mode == "lost-owner" {
				current.ActiveOwner = "replacement"
			}
			if mode == "expired" {
				current.LeaseUntil = time.Now().Add(-time.Minute)
			}
			registrationUID := "registration"
			if mode == "changed-uid" {
				registrationUID = "replacement"
			}
			labels := map[string]string{"app.kubernetes.io/instance": registration.Release, "app.kubernetes.io/component": "backend"}
			if mode == "foreign-deployment" {
				labels["app.kubernetes.io/instance"] = "foreign"
			}
			fixtures := map[string]any{
				"registration": map[string]any{"metadata": map[string]any{"uid": registrationUID, "labels": map[string]string{"registry-hub.io/test-id": registration.ID, "registry-hub.io/managed-by": runnerManagedBy}}, "data": registrationData(current)},
				"deployments":  map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"name": registration.Fullname, "uid": "deployment", "resourceVersion": "42", "labels": labels}, "spec": map[string]any{"replicas": 1, "selector": map[string]any{"matchLabels": labels}}}}},
			}
			data, err := json.Marshal(fixtures)
			if err != nil {
				t.Fatal(err)
			}
			writeLeaseFixture(t, filepath.Join(directory, "fixture.json"), data, 0600)
			writeLeaseFixture(t, filepath.Join(directory, "kubectl"), []byte(pauseKubectlFixture), 0700)
			t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("PAUSE_FIXTURE", directory)
			err = (Environment{}).Pause(context.Background(), registration, "registration")
			if (err != nil) != (mode != "owned") {
				t.Fatalf("pause mode=%s err=%v", mode, err)
			}
			data, readErr := os.ReadFile(filepath.Join(directory, "patch.json"))
			if mode != "owned" {
				if !os.IsNotExist(readErr) {
					t.Fatalf("unowned run was patched: %s %v", data, readErr)
				}
				return
			}
			var patch []map[string]any
			if readErr != nil || json.Unmarshal(data, &patch) != nil || len(patch) != 4 || patch[0]["value"] != "deployment" || patch[1]["value"] != "42" || patch[3]["value"] != float64(0) {
				t.Fatalf("unsafe pause patch=%s err=%v", data, readErr)
			}
			selector, err := os.ReadFile(filepath.Join(directory, "pods-selector"))
			if err != nil || string(selector) != "app.kubernetes.io/component=backend,app.kubernetes.io/instance=rh-min-pause" {
				t.Fatalf("wait escaped deployment selector: %s %v", selector, err)
			}
		})
	}
}

const pauseKubectlFixture = `#!/usr/bin/env python3
import json, os, pathlib, sys
root = pathlib.Path(os.environ["PAUSE_FIXTURE"])
fixture = json.loads((root / "fixture.json").read_text())
args = sys.argv[1:]
if args[2] == "get":
    if args[3] == "configmap":
        print(json.dumps(fixture["registration"]))
    elif args[3] == "deployment":
        deployment = fixture["deployments"]["items"][0]
        deployment["spec"]["replicas"] = 0
        print(json.dumps(deployment))
    elif args[3] == "pods":
        (root / "pods-selector").write_text(args[args.index("-l") + 1])
        print(json.dumps({"items": [{"metadata": {"name": "standalone-backend-probe"}}]}))
    else:
        print(json.dumps(fixture["deployments"]))
elif args[2] == "patch":
    (root / "patch.json").write_text(args[args.index("-p") + 1])
else:
    sys.exit(1)
`
