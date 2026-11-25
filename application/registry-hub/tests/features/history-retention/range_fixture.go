package historyretention

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

const rangeFixtureEvents = 38

func seedRangeEvents(ctx context.Context, env testkit.CaseContext, from time.Time) error {
	deployment, err := testkit.KubectlGet(ctx, env.Namespace, "deployment", env.Fullname)
	if err != nil {
		return err
	}
	deploymentSpec := testkit.Map(deployment["spec"])
	labels := testkit.Map(testkit.Map(deploymentSpec["selector"])["matchLabels"])
	selectors := make([]string, 0, len(labels))
	for key, value := range labels {
		selectors = append(selectors, key+"="+testkit.String(value))
	}
	if len(selectors) == 0 {
		return fmt.Errorf("isolated backend has no pod selector")
	}
	pods, err := testkit.KubectlList(ctx, env.Namespace, "get", "pods", "-l", strings.Join(selectors, ","))
	if err != nil || len(pods) != 1 {
		return fmt.Errorf("find isolated backend pod: count=%d error=%v", len(pods), err)
	}
	spec := testkit.Map(testkit.Map(deploymentSpec["template"])["spec"])
	name := env.Release + "-history-seed"
	image := os.Getenv("REGISTRY_HUB_TEST_PROBE_IMAGE")
	if image == "" {
		image = "container-registry-mirror.internal.home.geekcity.tech:32443/docker.io/curlimages/curl:8.10.1"
	}
	manifest := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": env.Namespace},
		"spec": map[string]any{
			"automountServiceAccountToken": false, "restartPolicy": "Never", "activeDeadlineSeconds": 90,
			"nodeName": testkit.String(testkit.Map(pods[0].Raw["spec"])["nodeName"]), "securityContext": spec["securityContext"],
			"containers": []map[string]any{{
				"name": "seed", "image": image,
				"command":         []string{"sh", "-ec", rangeSeedScript(from)},
				"securityContext": map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []string{"ALL"}}},
				"volumeMounts":    []map[string]any{{"name": "state", "mountPath": "/state"}},
			}},
			"volumes": []map[string]any{{"name": "state", "persistentVolumeClaim": map[string]any{"claimName": env.Fullname + "-backend"}}},
		},
	}
	content, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if _, err := testkit.CommandInput(ctx, content, "kubectl", "create", "-f", "-"); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		_, _ = testkit.Command(cleanup, "kubectl", "-n", env.Namespace, "delete", "pod", name, "--ignore-not-found", "--wait=false")
	}()
	if _, err := testkit.Command(ctx, "kubectl", "-n", env.Namespace, "wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/"+name, "--timeout=70s"); err != nil {
		logs, _ := testkit.Command(ctx, "kubectl", "-n", env.Namespace, "logs", name, "--tail=20")
		return fmt.Errorf("seed range events: %w: %s", err, logs.Stdout)
	}
	return nil
}

func rangeSeedScript(from time.Time) string {
	var script strings.Builder
	for i := -1; i < rangeFixtureEvents; i++ {
		at := from.Add(time.Duration(i) * time.Second)
		id := fmt.Sprintf("range-fixture-%02d", i+1)
		dir := "/state/diagnostics/v1/events/" + at.Format("20060102")
		fmt.Fprintf(&script, "mkdir -p '%s'\n", dir)
		fmt.Fprintf(&script, "printf '%%s\\n' '{\"id\":\"%s\",\"occurredAt\":\"%s\"}' > '%s/%s-%s.json'\n",
			id, at.Format(time.RFC3339Nano), dir, at.Format("150405.000000000"), id)
	}
	return script.String()
}
