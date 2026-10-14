package startupwait

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"path/filepath"
	"time"
)

//go:embed case.json
var metadata embed.FS

type recordSnapshot struct {
	Name      string `json:"name"`
	UID       string `json:"uid"`
	ManagedBy string `json:"managedBy"`
	Content   string `json:"content"`
}

type recoveryJournal struct {
	Records []recordSnapshot `json:"records"`
}

const (
	initializationProjectionTimeout = 120 * time.Second
)

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "startup-wait")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	objects, err := support.GetGeneratedObjects(ctx, environment.Namespace, environment.Fullname)
	if err != nil {
		return err
	}
	internal, err := support.FindObject(objects, "ConfigMap", "internal-initializer")
	if err != nil {
		return err
	}
	admin, err := support.FindObject(objects, "ConfigMap", "admin-initializer")
	if err != nil {
		return err
	}
	journalPath := filepath.Join(environment.StateDir, "startup-wait-recovery.json")
	if err := recoverInterruptedRun(ctx, environment, journalPath, internal, admin); err != nil {
		return err
	}
	objects, err = support.GetGeneratedObjects(ctx, environment.Namespace, environment.Fullname)
	if err != nil {
		return err
	}
	internal, err = support.FindObject(objects, "ConfigMap", "internal-initializer")
	if err != nil {
		return err
	}
	admin, err = support.FindObject(objects, "ConfigMap", "admin-initializer")
	if err != nil {
		return err
	}
	internalState, err := recordState(internal)
	if err != nil {
		return err
	}
	adminState, err := recordState(admin)
	if err != nil {
		return err
	}
	if internalState != "ready" || adminState != "ready" {
		return fmt.Errorf("startup-wait requires both initialization records to be ready")
	}
	journal := recoveryJournal{Records: []recordSnapshot{
		snapshotRecord(internal, "internal-initializer"),
		snapshotRecord(admin, "admin-initializer"),
	}}
	if err := testkit.WriteJSONAtomic(journalPath, journal); err != nil {
		return err
	}
	completed := false
	defer func() {
		if completed {
			return
		}
		recoveryContext, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		_ = restoreJournal(recoveryContext, environment, journalPath, journal)
	}()
	observations := map[string]any{}
	if err := exerciseWait(ctx, environment, internal, "registry", environment.Fullname+"-registry", "wait-internal-initialization", "registry", "internal"); err != nil {
		return err
	}
	observations["internal"] = map[string]any{"waitContainer": "running", "mainContainerStarted": false}
	if err := exerciseWait(ctx, environment, admin, "backend", environment.Fullname, "wait-admin-auth-initialization", "backend", "admin"); err != nil {
		return err
	}
	observations["admin"] = map[string]any{"waitContainer": "running", "mainContainerStarted": false}
	observations["restored"] = "both deployments Ready"
	if err := restoreJournal(ctx, environment, journalPath, journal); err != nil {
		return err
	}
	completed = true
	return support.WriteResult(environment, observations)
}

func exerciseWait(ctx context.Context, environment testkit.CaseContext, record testkit.Object, component, deployment, waitContainer, mainContainer, label string) error {
	original := testkit.String(record.Data["record.json"])
	pending, err := withRecordState(original, "pending")
	if err != nil {
		return err
	}
	restored := false
	defer func() {
		if restored {
			return
		}
		restoreContext, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = patchRecord(restoreContext, environment.Namespace, record.Name, original)
	}()
	if err := patchRecord(ctx, environment.Namespace, record.Name, pending); err != nil {
		return err
	}

	pods, err := listComponentPods(ctx, environment, component)
	if err != nil {
		return err
	}
	if len(pods) != 1 {
		return fmt.Errorf("expected one %s Pod before restart, found %d", component, len(pods))
	}
	previousUID := pods[0].UID
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "delete", "pod", pods[0].Name, "--wait=true", "--timeout=30s"); err != nil {
		return err
	}
	if err := waitForBlockedPod(ctx, environment, component, previousUID, waitContainer, mainContainer); err != nil {
		return fmt.Errorf("%s initialization wait was not observed: %w", label, err)
	}
	if err := patchRecord(ctx, environment.Namespace, record.Name, original); err != nil {
		return err
	}
	restored = true
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "status", "deployment/"+deployment, "--timeout=180s"); err != nil {
		return fmt.Errorf("%s workload did not recover after initialization record became ready: %w", label, err)
	}
	return nil
}

func listComponentPods(ctx context.Context, environment testkit.CaseContext, component string) ([]testkit.Object, error) {
	selector := "app.kubernetes.io/instance=" + environment.Release + ",app.kubernetes.io/component=" + component
	objects, err := testkit.KubectlList(ctx, environment.Namespace, "get", "pods", "-l", selector)
	if err != nil {
		return nil, err
	}
	pods := make([]testkit.Object, 0, len(objects))
	for _, object := range objects {
		if replicaSetControlled(object) {
			pods = append(pods, object)
		}
	}
	return pods, nil
}

func replicaSetControlled(pod testkit.Object) bool {
	for _, raw := range testkit.Slice(pod.Metadata["ownerReferences"]) {
		owner := testkit.Map(raw)
		controller, _ := owner["controller"].(bool)
		if testkit.String(owner["kind"]) == "ReplicaSet" && controller {
			return true
		}
	}
	return false
}

func waitForBlockedPod(ctx context.Context, environment testkit.CaseContext, component, previousUID, waitContainer, mainContainer string) error {
	deadline := time.NewTimer(initializationProjectionTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		pods, err := listComponentPods(ctx, environment, component)
		if err == nil {
			for _, pod := range pods {
				if pod.UID == previousUID {
					continue
				}
				if blockedOnInitialization(pod.Raw, waitContainer, mainContainer) {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timed out waiting for init container %s", waitContainer)
		case <-ticker.C:
		}
	}
}

func blockedOnInitialization(pod map[string]any, waitContainer, mainContainer string) bool {
	status := testkit.Map(pod["status"])
	initStatuses := testkit.Slice(status["initContainerStatuses"])
	waitRunning := false
	for _, raw := range initStatuses {
		container := testkit.Map(raw)
		if testkit.String(container["name"]) == waitContainer && len(testkit.Map(testkit.Map(container["state"])["running"])) > 0 {
			waitRunning = true
		}
	}
	if !waitRunning {
		return false
	}
	for _, raw := range testkit.Slice(status["containerStatuses"]) {
		container := testkit.Map(raw)
		if testkit.String(container["name"]) == mainContainer && len(testkit.Map(testkit.Map(container["state"])["running"])) > 0 {
			return false
		}
	}
	return true
}

func recordState(record testkit.Object) (string, error) {
	var values map[string]any
	if err := json.Unmarshal([]byte(testkit.String(record.Data["record.json"])), &values); err != nil {
		return "", fmt.Errorf("initialization record %s is invalid", record.Name)
	}
	return testkit.String(values["state"]), nil
}
