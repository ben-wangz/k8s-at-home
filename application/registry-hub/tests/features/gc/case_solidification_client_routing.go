package gc

import (
	"context"
	"sort"
	"strings"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func clientRoutingEvidence(ctx context.Context, environment testkit.CaseContext) map[string]any {
	evidence := map[string]any{}
	service, err := testkit.KubectlGet(ctx, environment.Namespace, "service", environment.Fullname)
	if err != nil {
		evidence["serviceError"] = err.Error()
		return evidence
	}
	spec := testkit.Map(service["spec"])
	selector := stringSelector(testkit.Map(spec["selector"]))
	evidence["service"] = map[string]any{
		"clusterIP": testkit.String(spec["clusterIP"]),
		"selector":  selector,
		"ports":     summarizeServicePorts(testkit.Slice(spec["ports"])),
	}

	endpointSlices, err := testkit.KubectlList(ctx, environment.Namespace, "get", "endpointslices", "-l", "kubernetes.io/service-name="+environment.Fullname)
	if err != nil {
		evidence["endpointSliceError"] = err.Error()
	} else {
		summaries := make([]map[string]any, 0, len(endpointSlices))
		for _, slice := range endpointSlices {
			summaries = append(summaries, summarizeEndpointSlice(slice.Raw))
		}
		evidence["endpointSlices"] = summaries
	}

	deployment, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname)
	if err != nil {
		evidence["deploymentError"] = err.Error()
	} else {
		evidence["deployment"] = summarizeDeployment(deployment)
	}
	if selector == "" {
		evidence["podError"] = "client Service has no selector"
		return evidence
	}
	pods, err := testkit.KubectlList(ctx, environment.Namespace, "get", "pods", "-l", selector)
	if err != nil {
		evidence["podError"] = err.Error()
	} else {
		summaries := make([]map[string]any, 0, len(pods))
		for _, pod := range pods {
			summaries = append(summaries, summarizeBackendPod(pod.Raw))
		}
		evidence["backendPods"] = summaries
	}
	return evidence
}

func stringSelector(values map[string]any) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value, ok := values[key].(string)
		if ok {
			parts = append(parts, key+"="+value)
		}
	}
	return strings.Join(parts, ",")
}

func summarizeServicePorts(raw []any) []map[string]any {
	ports := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		port := testkit.Map(item)
		ports = append(ports, map[string]any{
			"name":       port["name"],
			"port":       port["port"],
			"targetPort": port["targetPort"],
		})
	}
	return ports
}

func summarizeEndpointSlice(slice map[string]any) map[string]any {
	summary := map[string]any{"name": testkit.String(testkit.Map(slice["metadata"])["name"])}
	endpoints := make([]map[string]any, 0)
	for _, raw := range testkit.Slice(slice["endpoints"]) {
		endpoint := testkit.Map(raw)
		conditions := testkit.Map(endpoint["conditions"])
		target := testkit.Map(endpoint["targetRef"])
		endpoints = append(endpoints, map[string]any{
			"addresses":   endpoint["addresses"],
			"ready":       conditions["ready"],
			"serving":     conditions["serving"],
			"terminating": conditions["terminating"],
			"targetKind":  target["kind"],
			"targetName":  target["name"],
		})
	}
	summary["endpoints"] = endpoints
	return summary
}

func summarizeDeployment(deployment map[string]any) map[string]any {
	spec := testkit.Map(deployment["spec"])
	status := testkit.Map(deployment["status"])
	return map[string]any{
		"replicas":            status["replicas"],
		"readyReplicas":       status["readyReplicas"],
		"availableReplicas":   status["availableReplicas"],
		"unavailableReplicas": status["unavailableReplicas"],
		"desiredReplicas":     spec["replicas"],
	}
}

func summarizeBackendPod(pod map[string]any) map[string]any {
	metadata := testkit.Map(pod["metadata"])
	spec := testkit.Map(pod["spec"])
	status := testkit.Map(pod["status"])
	conditions := make([]map[string]any, 0)
	for _, raw := range testkit.Slice(status["conditions"]) {
		condition := testkit.Map(raw)
		if testkit.String(condition["type"]) == "Ready" {
			conditions = append(conditions, map[string]any{"type": condition["type"], "status": condition["status"], "reason": condition["reason"]})
		}
	}
	containers := make([]map[string]any, 0)
	for _, raw := range testkit.Slice(status["containerStatuses"]) {
		container := testkit.Map(raw)
		containers = append(containers, map[string]any{
			"name":         container["name"],
			"ready":        container["ready"],
			"restartCount": container["restartCount"],
		})
	}
	return map[string]any{
		"name":              metadata["name"],
		"phase":             status["phase"],
		"podIP":             status["podIP"],
		"nodeName":          spec["nodeName"],
		"deletionTimestamp": testkit.Map(metadata)["deletionTimestamp"],
		"readyConditions":   conditions,
		"containers":        containers,
	}
}
