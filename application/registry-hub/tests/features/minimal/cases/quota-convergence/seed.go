package quotaconvergence

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func seedHistoricalTasks(ctx context.Context, environment testkit.CaseContext) (runErr error) {
	if environment.Namespace != "develop" {
		return fmt.Errorf("quota fixture requires develop namespace")
	}
	deployment, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname)
	if err != nil {
		return err
	}
	spec := testkit.Map(deployment["spec"])
	labels := testkit.Map(testkit.Map(deployment["metadata"])["labels"])
	if testkit.String(labels["app.kubernetes.io/instance"]) != environment.Release || testkit.String(testkit.Map(spec["strategy"])["type"]) != "Recreate" {
		return fmt.Errorf("quota fixture requires its owned Recreate deployment")
	}
	pod, err := testkit.KubectlGet(ctx, environment.Namespace, "pod", environment.Release+"-external-upstream-proxy")
	if err != nil {
		return err
	}
	containers := testkit.Slice(testkit.Map(pod["spec"])["containers"])
	if len(containers) != 1 {
		return fmt.Errorf("quota fixture proxy image is unavailable")
	}
	name := environment.Release + "-external-quota-seed"
	fixture := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": name, "namespace": environment.Namespace, "labels": map[string]string{"app.kubernetes.io/instance": environment.Release, "registry-hub.io/test-id": environment.RunID, "registry-hub.io/instance": environment.Fullname, "registry-hub.io/managed-by": "registry-hub-test-fixture"}}, "data": historicalTaskRecords(environment.RunID)}
	encoded, err := json.Marshal(fixture)
	if err != nil {
		return err
	}
	if _, err := testkit.CommandInput(ctx, encoded, "kubectl", "-n", environment.Namespace, "apply", "-f", "-"); err != nil {
		return err
	}
	seeder := map[string]any{
		"name": "quota-seed", "image": testkit.Map(containers[0])["image"], "imagePullPolicy": "IfNotPresent",
		"command":         []string{"/upstream-fixture", "seed-tasks", "/seed", "/state/tasks/state"},
		"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532, "allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []string{"ALL"}}},
		"volumeMounts":    []map[string]any{{"name": "backend-data", "mountPath": "/state"}, {"name": "quota-seed", "mountPath": "/seed", "readOnly": true}},
	}
	defer func() {
		restore, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Minute)
		defer cancel()
		runErr = errors.Join(runErr, patchSeedPod(restore, environment, map[string]any{"initContainers": []map[string]any{{"name": "quota-seed", "$patch": "delete"}}, "volumes": []map[string]any{{"name": "quota-seed", "$patch": "delete"}}}))
	}()
	return patchSeedPod(ctx, environment, map[string]any{"initContainers": []map[string]any{seeder}, "volumes": []map[string]any{{"name": "quota-seed", "configMap": map[string]any{"name": name}}}})
}

func patchSeedPod(ctx context.Context, environment testkit.CaseContext, spec map[string]any) error {
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": spec}}})
	if err != nil {
		return err
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "patch", "deployment", environment.Fullname, "--type=strategic", "-p", string(patch)); err != nil {
		return err
	}
	_, err = testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "status", "deployment/"+environment.Fullname, "--timeout=180s")
	return err
}

func historicalTaskRecords(runID string) map[string]string {
	result := map[string]string{}
	now := time.Now().UTC()
	for n := range 100 {
		reference := fmt.Sprintf("quota-unrelated-%s-%03d", runID, n)
		digest := sha256.Sum256([]byte(reference))
		id := fmt.Sprintf("%x", digest[:16])
		task := map[string]any{"schemaVersion": 3, "protocolVersion": 2, "id": id, "roundID": id, "kind": "root", "authority": "github.com", "repository": "registry-hub-test/fixture", "reference": reference, "phase": "discovery", "status": "failed", "terminalCleanupComplete": true, "createdAt": now.Add(-time.Hour), "updatedAt": now.Add(-time.Hour), "lastProgressAt": now.Add(-time.Hour), "expiresAt": now.Add(24 * time.Hour)}
		encoded, _ := json.Marshal(task)
		result[id+".json"] = string(encoded)
	}
	return result
}
