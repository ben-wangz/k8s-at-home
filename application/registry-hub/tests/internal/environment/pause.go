package environment

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func (environment Environment) Pause(ctx context.Context, registration Registration, registrationUID string) error {
	if err := ValidateRegistration(registration, registration.ID, registration.KubeContext); err != nil {
		return err
	}
	objects, err := testkit.KubectlJSON(ctx, Namespace, "get", "deployments", "-l", "app.kubernetes.io/instance="+registration.Release)
	if err != nil {
		return err
	}
	deployments := []pausedDeployment{}
	for _, raw := range testkit.Slice(objects["items"]) {
		current, identity, err := environment.GetRegistration(ctx, registration.ID)
		if err != nil {
			return err
		}
		if identity.UID != registrationUID || current.ActiveOwner == "" || current.ActiveOwner != registration.ActiveOwner || !current.LeaseUntil.After(time.Now().UTC()) {
			return fmt.Errorf("test ID %s lease or identity changed; refusing to pause", registration.ID)
		}
		object := testkit.Map(raw)
		metadata := testkit.Map(object["metadata"])
		name := testkit.String(metadata["name"])
		uid := testkit.String(metadata["uid"])
		version := testkit.String(metadata["resourceVersion"])
		labels := testkit.Map(metadata["labels"])
		if uid == "" || version == "" || (name != registration.Fullname && !strings.HasPrefix(name, registration.Fullname+"-")) || testkit.String(labels["app.kubernetes.io/instance"]) != registration.Release {
			return fmt.Errorf("deployment %s has unexpected ownership; refusing to pause", name)
		}
		matchLabels := testkit.Map(testkit.Map(testkit.Map(object["spec"])["selector"])["matchLabels"])
		if testkit.String(matchLabels["app.kubernetes.io/instance"]) != registration.Release {
			return fmt.Errorf("deployment %s has an unexpected pod selector", name)
		}
		selector := []string{}
		for key, value := range matchLabels {
			selector = append(selector, key+"="+testkit.String(value))
		}
		sort.Strings(selector)
		deployments = append(deployments, pausedDeployment{name: name, uid: uid, selector: strings.Join(selector, ",")})
		patch, err := json.Marshal([]map[string]any{
			{"op": "test", "path": "/metadata/uid", "value": uid},
			{"op": "test", "path": "/metadata/resourceVersion", "value": version},
			{"op": "test", "path": "/metadata/labels/app.kubernetes.io~1instance", "value": registration.Release},
			{"op": "replace", "path": "/spec/replicas", "value": 0},
		})
		if err != nil {
			return err
		}
		if _, err := testkit.Command(ctx, "kubectl", "-n", Namespace, "patch", "deployment", name, "--type=json", "-p", string(patch)); err != nil {
			return err
		}
	}
	for _, deployment := range deployments {
		if err := environment.waitPausedDeployment(ctx, deployment); err != nil {
			return err
		}
	}
	return nil
}

type pausedDeployment struct {
	name     string
	uid      string
	selector string
}

func (environment Environment) waitPausedDeployment(ctx context.Context, deployment pausedDeployment) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		object, err := testkit.KubectlGet(ctx, Namespace, "deployment", deployment.name)
		if err != nil {
			return err
		}
		metadata := testkit.Map(object["metadata"])
		if testkit.String(metadata["uid"]) != deployment.uid || testkit.Int(testkit.Map(object["spec"])["replicas"]) != 0 {
			return fmt.Errorf("deployment %s changed while pausing", deployment.name)
		}
		pods, err := testkit.KubectlJSON(ctx, Namespace, "get", "pods", "-l", deployment.selector)
		if err != nil {
			return err
		}
		remaining := false
		for _, raw := range testkit.Slice(pods["items"]) {
			owners := testkit.Slice(testkit.Map(testkit.Map(raw)["metadata"])["ownerReferences"])
			for _, rawOwner := range owners {
				owner := testkit.Map(rawOwner)
				if owner["controller"] == true && testkit.String(owner["kind"]) == "ReplicaSet" && strings.HasPrefix(testkit.String(owner["name"]), deployment.name+"-") {
					remaining = true
				}
			}
		}
		status := testkit.Map(object["status"])
		if !remaining && testkit.Int(status["observedGeneration"]) >= testkit.Int(metadata["generation"]) && testkit.Int(status["replicas"]) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
