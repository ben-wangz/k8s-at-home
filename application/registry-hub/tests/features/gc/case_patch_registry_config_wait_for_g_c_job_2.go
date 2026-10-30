package gc

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"time"
)

func waitForGCJob(ctx context.Context, environment testkit.CaseContext, expectedID string) (map[string]any, error) {
	for {
		response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/gc", nil, nil)
		if err != nil {
			return nil, err
		}
		var status map[string]any
		if _, err := support.DecodeJSON(response, 2<<20, &status); err != nil {
			return nil, err
		}
		run := testkit.Map(status["run"])
		state := testkit.String(run["state"])
		if testkit.String(run["id"]) != expectedID {
			return nil, fmt.Errorf("GC run identity changed before Job creation")
		}
		if testkit.String(run["jobName"]) != "" && testkit.String(run["jobUID"]) != "" {
			return run, nil
		}
		if state == "failed" || state == "failed_unknown" || state == "succeeded" {
			return nil, fmt.Errorf("GC reached terminal state %s before a Job identity was recorded", state)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func setGCJobSuspended(ctx context.Context, environment testkit.CaseContext, jobName string, suspended bool) error {
	patch, err := json.Marshal(map[string]any{"spec": map[string]bool{"suspend": suspended}})
	if err != nil {
		return err
	}
	_, err = testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "patch", "job", jobName, "--type=merge", "-p", string(patch))
	return err
}

func restartBackendPod(ctx context.Context, environment testkit.CaseContext) error {
	deployment, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname)
	if err != nil {
		return err
	}
	selector := testkit.Map(testkit.Map(testkit.Map(deployment["spec"])["selector"])["matchLabels"])
	pods, err := testkit.KubectlList(ctx, environment.Namespace, "get", "pods")
	if err != nil {
		return err
	}
	backendPod := ""
	for _, pod := range pods {
		labels := testkit.Map(testkit.Map(pod.Raw["metadata"])["labels"])
		owners := testkit.Slice(testkit.Map(pod.Raw["metadata"])["ownerReferences"])
		if matchesLabels(labels, selector) && hasControllerKind(owners, "ReplicaSet") {
			backendPod = pod.Name
			break
		}
	}
	if backendPod == "" {
		return fmt.Errorf("backend Deployment has no ReplicaSet-owned Pod to restart")
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "delete", "pod", backendPod, "--wait=true", "--timeout=90s"); err != nil {
		return fmt.Errorf("restart backend Pod %s: %w", backendPod, err)
	}
	// Unresolved GC deliberately keeps readiness closed while the admin listener recovers.
	readyCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	for {
		response, err := support.AdminRequest(readyCtx, environment, http.MethodGet, "/healthz", nil, nil)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-readyCtx.Done():
			return fmt.Errorf("backend admin listener did not recover after restarting %s: %w", backendPod, readyCtx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func hasControllerKind(references []any, kind string) bool {
	for _, raw := range references {
		owner := testkit.Map(raw)
		if testkit.String(owner["kind"]) == kind && owner["controller"] == true {
			return true
		}
	}
	return false
}
