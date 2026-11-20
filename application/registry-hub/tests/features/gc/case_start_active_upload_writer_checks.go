package gc

import (
	"context"
	"fmt"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func verifyPVCConsumers(ctx context.Context, environment testkit.CaseContext, gcJobName string) error {
	claimName := environment.Fullname + "-registry"
	pods, err := testkit.KubectlList(ctx, environment.Namespace, "get", "pods")
	if err != nil {
		return err
	}
	registryDeployment, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", claimName)
	if err != nil {
		return err
	}
	registrySelector := testkit.Map(testkit.Map(testkit.Map(registryDeployment["spec"])["selector"])["matchLabels"])
	registryConsumers, jobConsumers := 0, 0
	for _, pod := range pods {
		podSpec := testkit.Map(pod.Raw["spec"])
		if !podClaims(podSpec, claimName) {
			continue
		}
		labels := testkit.Map(testkit.Map(pod.Raw["metadata"])["labels"])
		if matchesLabels(labels, registrySelector) {
			registryConsumers++
			continue
		}
		ownerReferences := testkit.Slice(testkit.Map(pod.Raw["metadata"])["ownerReferences"])
		if gcJobName != "" && hasJobOwner(ownerReferences, gcJobName) {
			jobConsumers++
			continue
		}
		return fmt.Errorf("unrecognized pod %s in %s mounts Registry PVC %s", pod.Name, environment.Namespace, claimName)
	}
	wantJobs := 0
	if gcJobName != "" {
		wantJobs = 1
	}
	if registryConsumers != 1 || jobConsumers != wantJobs {
		return fmt.Errorf("Registry PVC %s consumers in %s: registry=%d GC job=%d, want 1/%d",
			claimName, environment.Namespace, registryConsumers, jobConsumers, wantJobs)
	}
	return nil
}

func verifyRegistryIngressPolicy(ctx context.Context, environment testkit.CaseContext) error {
	backend, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname)
	if err != nil {
		return err
	}
	registry, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname+"-registry")
	if err != nil {
		return err
	}
	policy, err := testkit.KubectlGet(ctx, environment.Namespace, "networkpolicy", environment.Fullname+"-registry")
	if err != nil {
		return err
	}
	backendSelector := testkit.Map(testkit.Map(testkit.Map(backend["spec"])["selector"])["matchLabels"])
	registrySelector := testkit.Map(testkit.Map(testkit.Map(registry["spec"])["selector"])["matchLabels"])
	spec := testkit.Map(policy["spec"])
	if !sameLabels(testkit.Map(testkit.Map(spec["podSelector"])["matchLabels"]), registrySelector) {
		return fmt.Errorf("Registry NetworkPolicy selector does not match its Deployment")
	}
	policyTypes := testkit.Slice(spec["policyTypes"])
	if !containsString(policyTypes, "Ingress") {
		return fmt.Errorf("Registry NetworkPolicy does not isolate ingress")
	}
	ingress := testkit.Slice(spec["ingress"])
	if len(ingress) != 1 {
		return fmt.Errorf("Registry NetworkPolicy must have one ingress rule")
	}
	rule := testkit.Map(ingress[0])
	from := testkit.Slice(rule["from"])
	ports := testkit.Slice(rule["ports"])
	if len(from) != 1 || !sameLabels(testkit.Map(testkit.Map(testkit.Map(from[0])["podSelector"])["matchLabels"]), backendSelector) ||
		len(ports) != 1 || testkit.String(testkit.Map(ports[0])["protocol"]) != "TCP" || testkit.Int(testkit.Map(ports[0])["port"]) != 5000 {
		return fmt.Errorf("Registry NetworkPolicy must allow only backend pods on TCP/5000")
	}
	return nil
}

func sameLabels(actual, expected map[string]any) bool {
	if len(actual) != len(expected) || len(actual) == 0 {
		return false
	}
	for key, value := range expected {
		if actual[key] != value {
			return false
		}
	}
	return true
}

func containsString(values []any, expected string) bool {
	for _, value := range values {
		if testkit.String(value) == expected {
			return true
		}
	}
	return false
}

func podClaims(podSpec map[string]any, claimName string) bool {
	for _, raw := range testkit.Slice(podSpec["volumes"]) {
		claim := testkit.String(testkit.Map(testkit.Map(raw)["persistentVolumeClaim"])["claimName"])
		if claim == claimName {
			return true
		}
	}
	return false
}

func matchesLabels(actual, selector map[string]any) bool {
	if len(selector) == 0 {
		return false
	}
	for key, value := range selector {
		if actual[key] != value {
			return false
		}
	}
	return true
}

func hasJobOwner(references []any, expectedName string) bool {
	for _, raw := range references {
		owner := testkit.Map(raw)
		if testkit.String(owner["kind"]) == "Job" && testkit.String(owner["name"]) == expectedName {
			return true
		}
	}
	return false
}

func registryConfigValue(ctx context.Context, environment testkit.CaseContext) (string, error) {
	configMap, err := testkit.KubectlGet(ctx, environment.Namespace, "configmap", environment.Fullname+"-registry-config")
	if err != nil {
		return "", err
	}
	config := testkit.String(testkit.Map(configMap["data"])["config.yml"])
	if config == "" {
		return "", fmt.Errorf("Registry config ConfigMap has no config.yml")
	}
	return config, nil
}
