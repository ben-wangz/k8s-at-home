package restartreuse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func captureSecrets(ctx context.Context, environment testkit.CaseContext) (map[string]snapshot, error) {
	result := make(map[string]snapshot, 4)
	for key, name := range map[string]string{
		"internal-secret": environment.Fullname + "-registry-internal",
		"admin-secret":    environment.Fullname + "-admin-auth",
	} {
		object, data, err := support.GetSecret(ctx, environment.Namespace, name)
		if err != nil {
			return nil, err
		}
		result[key] = snapshot{UID: object.UID, Hashes: support.SecretHashes(data)}
	}
	generated, err := support.GetGeneratedObjects(ctx, environment.Namespace, environment.Fullname)
	if err != nil {
		return nil, err
	}
	for key, managedBy := range map[string]string{
		"internal-record": "internal-initializer",
		"admin-record":    "admin-initializer",
	} {
		object, err := support.FindObject(generated, "ConfigMap", managedBy)
		if err != nil {
			return nil, err
		}
		state, err := json.Marshal(object.Data)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(state)
		result[key] = snapshot{UID: object.UID, StateSHA256: hex.EncodeToString(digest[:])}
	}
	return result, nil
}

func equalSnapshots(left, right map[string]snapshot) bool {
	for key, before := range left {
		after, exists := right[key]
		if !exists || before.UID != after.UID || before.StateSHA256 != after.StateSHA256 || len(before.Hashes) != len(after.Hashes) {
			return false
		}
		for name, digest := range before.Hashes {
			if after.Hashes[name] != digest {
				return false
			}
		}
	}
	return true
}

func deleteInitializationJob(ctx context.Context, environment testkit.CaseContext) (string, error) {
	selector := "app.kubernetes.io/instance=" + environment.Release + ",app.kubernetes.io/component=internal-initialization"
	jobs, err := testkit.KubectlList(ctx, environment.Namespace, "get", "jobs", "-l", selector)
	if err != nil {
		return "", err
	}
	if len(jobs) != 1 || jobs[0].Name == "" || jobs[0].UID == "" {
		return "", fmt.Errorf("expected one owned initialization Job with a UID")
	}
	job, err := testkit.KubectlGet(ctx, environment.Namespace, "job", jobs[0].Name)
	if err != nil {
		return "", err
	}
	metadata := testkit.Map(job["metadata"])
	labels := testkit.Map(metadata["labels"])
	if testkit.String(metadata["uid"]) != jobs[0].UID ||
		testkit.String(labels["app.kubernetes.io/instance"]) != environment.Release ||
		testkit.String(labels["app.kubernetes.io/component"]) != "internal-initialization" {
		return "", fmt.Errorf("initialization Job ownership changed; refusing deletion")
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "delete", "job", jobs[0].Name, "--wait=true", "--timeout=60s"); err != nil {
		return "", err
	}
	return jobs[0].UID, nil
}

func verifyRecreatedInitializationJob(ctx context.Context, environment testkit.CaseContext, oldUID string) (string, error) {
	selector := "app.kubernetes.io/instance=" + environment.Release + ",app.kubernetes.io/component=internal-initialization"
	jobs, err := testkit.KubectlList(ctx, environment.Namespace, "get", "jobs", "-l", selector)
	if err != nil {
		return "", err
	}
	if len(jobs) != 1 || jobs[0].UID == "" || jobs[0].UID == oldUID ||
		testkit.Int(testkit.Map(jobs[0].Raw["status"])["succeeded"]) != 1 {
		return "", fmt.Errorf("initialization Job was not recreated successfully")
	}
	return jobs[0].UID, nil
}

func renderChart(ctx context.Context, environment testkit.CaseContext) (string, error) {
	result, err := testkit.Command(ctx, "helm", "template", environment.Release, environment.ChartDir,
		"--namespace", environment.Namespace,
		"--values", environment.BaseValues,
		"--values", environment.ValuesFile)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(result.Stdout)
	return hex.EncodeToString(digest[:]), nil
}
