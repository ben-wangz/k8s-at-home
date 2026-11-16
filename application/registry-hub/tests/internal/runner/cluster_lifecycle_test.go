package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/environment"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func TestClusterFailedEnvironmentPausesAndExpires(t *testing.T) {
	if os.Getenv("REGISTRY_HUB_CLUSTER_LIFECYCLE_TEST") != "1" {
		t.Skip("set REGISTRY_HUB_CLUSTER_LIFECYCLE_TEST=1 to use the develop cluster")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	env := environment.Environment{}
	contextName, err := env.CurrentContext(ctx)
	if err != nil || contextName != "develop" {
		t.Fatalf("cluster check requires the develop context: %s %v", contextName, err)
	}
	id, err := testkit.GenerateRunID()
	if err != nil {
		t.Fatal(err)
	}
	release, err := environment.ReleaseName("feature-minimal", id)
	if err != nil {
		t.Fatal(err)
	}
	registration := environment.Registration{ID: id, ScenarioID: "feature-minimal", Release: release, Fullname: release + "-registry-hub", KubeContext: contextName, ActiveOwner: "lifecycle-test", LeaseUntil: time.Now().Add(environment.RunnerLeaseDuration), StartedAt: time.Now(), State: "running"}
	identity, err := env.Reserve(ctx, registration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 2*time.Minute)
		defer stop()
		if _, _, err := env.GetRegistration(cleanup, id); testkit.IsNotFound(err) {
			return
		}
		if _, err := env.Destroy(cleanup, registration, identity.UID); err != nil {
			t.Errorf("remove cluster fixture %s: %v", id, err)
		}
	})
	chart := writeLifecycleChart(t, registration)
	if _, err := testkit.Command(ctx, "helm", "install", release, chart, "--namespace", environment.Namespace, "--wait=false"); err != nil {
		t.Fatal(err)
	}
	waitForLifecyclePod(t, ctx, release)
	runtime := &Runtime{Environment: env, StateRoot: t.TempDir()}
	stateDir := scenarioStateDir(runtime.StateRoot, registration.ScenarioID, id)
	report := testkit.RunRecord{ID: id, ScenarioID: registration.ScenarioID, Release: release, Fullname: registration.Fullname, Namespace: environment.Namespace, KubeContext: contextName, StartedAt: registration.StartedAt}
	cause := errors.New("expected cluster lifecycle failure")
	if err := runtime.preserveRunFailure(cause, "failed", stateDir, registration, identity.UID, &report); !errors.Is(err, cause) || report.PauseError != "" || !report.WorkloadsPaused {
		t.Fatalf("failure finalization lost its cause or did not pause: report=%+v err=%v", report, err)
	}
	deployment, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", registration.Fullname)
	if err != nil || testkit.Int(testkit.Map(deployment["spec"])["replicas"]) != 0 {
		t.Fatalf("deployment still active: %v", err)
	}
	if _, err := testkit.KubectlGet(ctx, environment.Namespace, "pvc", registration.Fullname+"-backend"); err != nil {
		t.Fatalf("failure did not retain the PVC: %v", err)
	}
	if err := env.ReleaseLease(ctx, id, registration.ActiveOwner); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano)
	patch, _ := json.Marshal(map[string]any{"data": map[string]string{"startedAt": old, "lastActivityAt": old}})
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "patch", "configmap", identity.Name, "--type=merge", "-p", string(patch)); err != nil {
		t.Fatal(err)
	}
	if err := runtime.destroy(ctx, id, FailedEnvironmentRetention); err != nil {
		t.Fatal(err)
	}
	for _, resource := range [][2]string{{"configmap", identity.Name}, {"deployment", registration.Fullname}, {"pvc", registration.Fullname + "-backend"}} {
		if _, err := testkit.KubectlGet(ctx, environment.Namespace, resource[0], resource[1]); !testkit.IsNotFound(err) {
			t.Fatalf("expired run still owns %v: %v", resource, err)
		}
	}
	preserved, err := readRecord(stateDir)
	if err != nil || preserved.State != "destroyed" || preserved.Failure != cause.Error() {
		t.Fatalf("destruction lost failure evidence: %+v %v", preserved, err)
	}
}

func waitForLifecyclePod(t *testing.T, ctx context.Context, release string) {
	t.Helper()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		pods, err := testkit.KubectlJSON(ctx, environment.Namespace, "get", "pods", "-l", "app.kubernetes.io/instance="+release)
		if err != nil {
			t.Fatal(err)
		}
		if len(testkit.Slice(pods["items"])) > 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func writeLifecycleChart(t *testing.T, registration environment.Registration) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "templates"), 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"Chart.yaml": "apiVersion: v2\nname: lifecycle-fixture\nversion: 0.0.0\n",
		"templates/resources.yaml": fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  labels: {app.kubernetes.io/instance: %[2]s}
spec:
  replicas: 1
  selector:
    matchLabels: {app.kubernetes.io/instance: %[2]s}
  template:
    metadata:
      labels: {app.kubernetes.io/instance: %[2]s}
    spec:
      terminationGracePeriodSeconds: 0
      containers:
        - name: sleeper
          image: container-registry-mirror.internal.home.geekcity.tech:32443/docker.io/library/busybox:1.37.0
          command: [sh, -c, "sleep 3600"]
          resources:
            requests: {cpu: 5m, memory: 8Mi}
            limits: {cpu: 50m, memory: 32Mi}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %[1]s-backend
  labels: {app.kubernetes.io/instance: %[2]s}
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests: {storage: 1Mi}
`, registration.Fullname, registration.Release),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}
