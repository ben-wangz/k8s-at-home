package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/environment"
)

func TestPruneDryRunAndRenewalRaceDoNotDestroyResources(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		t.Run(map[bool]string{true: "dry-run", false: "renewal race"}[dryRun], func(t *testing.T) {
			directory := t.TempDir()
			old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano)
			object := func(id, state string) map[string]any {
				return map[string]any{
					"metadata": map[string]any{"name": environment.RegistrationName(id), "uid": id, "resourceVersion": "42", "labels": map[string]string{"registry-hub.io/managed-by": "registry-hub-test-runner", "registry-hub.io/test-id": id}},
					"data":     map[string]any{"id": id, "scenarioId": "feature-minimal", "release": "rh-min-" + id, "fullname": "rh-min-" + id + "-registry-hub", "kubeContext": "test", "state": state, "startedAt": old},
				}
			}
			expired := object("expired", "failed")
			recent := object("expired", "failed")
			recent["data"].(map[string]any)["lastActivityAt"] = time.Now().UTC().Format(time.RFC3339Nano)
			payload := map[string]any{"list": map[string]any{"items": []any{expired, object("excluded", "failed"), object("intentional", "environment-ready")}}, "current": recent}
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			for name, data := range map[string][]byte{"fixture.json": encoded, "kubectl": []byte(pruneKubectlFixture), "helm": []byte("#!/bin/sh\nexit 1\n")} {
				if err := os.WriteFile(filepath.Join(directory, name), data, 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("PRUNE_FIXTURE", directory)
			output, err := os.Create(filepath.Join(directory, "output"))
			if err != nil {
				t.Fatal(err)
			}
			previous := os.Stdout
			os.Stdout = output
			t.Cleanup(func() { os.Stdout = previous; _ = output.Close() })
			runtime := &Runtime{Environment: environment.Environment{}, StateRoot: filepath.Join(directory, "state")}
			if err := runtime.Prune(context.Background(), 24*time.Hour, dryRun, "excluded"); err != nil {
				t.Fatal(err)
			}
			os.Stdout = previous
			data, err := os.ReadFile(output.Name())
			if err != nil {
				t.Fatal(err)
			}
			if dryRun && (strings.Count(string(data), "Expired test environment:") != 1 || !strings.Contains(string(data), "expired (feature-minimal, failed)")) {
				t.Fatalf("dry run selected the wrong environments: %s", data)
			}
		})
	}
}

const pruneKubectlFixture = `#!/usr/bin/env python3
import json, os, pathlib, sys
fixture = json.loads((pathlib.Path(os.environ["PRUNE_FIXTURE"]) / "fixture.json").read_text())
args = sys.argv[1:]
if args == ["config", "current-context"]:
    print("test")
elif args[2:4] == ["get", "configmaps"]:
    print(json.dumps(fixture["list"]))
elif args[2:4] == ["get", "configmap"]:
    print(json.dumps(fixture["current"]))
else:
    sys.exit("unexpected mutation: " + " ".join(args))
`
