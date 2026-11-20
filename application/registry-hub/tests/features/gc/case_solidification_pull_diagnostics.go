package gc

import (
	"context"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func podmanPullFailureDiagnostics(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, repository string, manifest support.Manifest, since time.Time, pullErr error) map[string]any {
	descriptors := make([]support.Descriptor, 0, len(manifest.Layers)+1)
	if manifest.Config != nil {
		descriptors = append(descriptors, *manifest.Config)
	}
	descriptors = append(descriptors, manifest.Layers...)
	digests := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		digests = append(digests, descriptor.Digest)
	}

	evidence := map[string]any{
		"pullError":                   pullErr.Error(),
		"evidenceWindowStart":         since.UTC().Format(time.RFC3339Nano),
		"manifestBlobs":               digests,
		"hubRequestLogsBeforeProbes":  []string{},
		"hubRequestLogsAfterProbes":   []string{},
		"fixtureBlobRequests":         map[string]fixtureBlobRequestEvidence{},
		"blobTasks":                   map[string]any{},
		"blobIntents":                 map[string]any{},
		"hubHeadResponses":            map[string]any{},
		"hubGetProbes":                map[string]any{},
		"hubGetProbeMayFetchAndCache": true,
		"clientRouting":               clientRoutingEvidence(ctx, environment),
	}
	requestLogs, err := backendRegistryRequestLogs(ctx, environment, since)
	if err != nil {
		evidence["hubRequestLogBeforeProbeError"] = err.Error()
	} else {
		evidence["hubRequestLogsBeforeProbes"] = requestLogs
	}
	fixtureRequests, err := fixtureBlobRequestEvidenceFor(ctx, environment, digests...)
	if err != nil {
		evidence["fixtureLogError"] = err.Error()
	} else {
		evidence["fixtureBlobRequests"] = fixtureRequests
	}

	blobTasks := make(map[string]any, len(descriptors))
	blobIntents := make(map[string]any, len(descriptors))
	headResponses := make(map[string]any, len(descriptors))
	getProbes := make(map[string]any, len(descriptors))
	for _, descriptor := range descriptors {
		digest := descriptor.Digest
		entry := map[string]any{"size": descriptor.Size}
		task, taskErr := taskDetailForBlobDigest(ctx, environment, repository, digest, since, "")
		if taskErr != nil {
			entry["taskError"] = taskErr.Error()
		} else {
			entry["task"] = summarizeBlobTask(task)
		}
		blobTasks[digest] = entry
		intentEvidence, intentErr := blobIntentEvidenceForDigest(ctx, environment, repository, digest)
		if intentErr != nil {
			blobIntents[digest] = map[string]any{"error": intentErr.Error()}
		} else {
			blobIntents[digest] = intentEvidence
		}

		response, headErr := registry.Head(ctx, repository, "/blobs/"+digest, nil)
		if headErr != nil {
			headResponses[digest] = map[string]any{"error": headErr.Error()}
		} else {
			headResponses[digest] = map[string]any{
				"status":              response.StatusCode,
				"contentLength":       response.Header.Get("Content-Length"),
				"contentType":         response.Header.Get("Content-Type"),
				"dockerContentDigest": response.Header.Get("Docker-Content-Digest"),
				"acceptRanges":        response.Header.Get("Accept-Ranges"),
			}
			_ = response.Body.Close()
		}

		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		getResponse, getErr := registry.Get(probeCtx, repository, "/blobs/"+digest, nil)
		if getErr != nil {
			getProbes[digest] = map[string]any{"error": getErr.Error()}
			cancel()
			continue
		}
		limit := int64(descriptor.Size) + 1
		if getResponse.StatusCode < 200 || getResponse.StatusCode >= 300 {
			limit = 4 << 10
		}
		if limit <= 0 || limit > 1<<20 {
			limit = 1 << 20
		}
		body, readErr := io.ReadAll(io.LimitReader(getResponse.Body, limit))
		_ = getResponse.Body.Close()
		cancel()
		probe := map[string]any{
			"status":              getResponse.StatusCode,
			"contentLength":       getResponse.Header.Get("Content-Length"),
			"dockerContentDigest": getResponse.Header.Get("Docker-Content-Digest"),
			"bytesRead":           len(body),
			"actualDigest":        support.Digest(body),
			"sizeMatches":         int64(len(body)) == int64(descriptor.Size),
			"digestMatches":       support.Digest(body) == digest,
		}
		if readErr != nil {
			probe["readError"] = readErr.Error()
		}
		if getResponse.StatusCode < 200 || getResponse.StatusCode >= 300 {
			probe["bodyPreview"] = strings.TrimSpace(string(body))
			probe["retryAfter"] = getResponse.Header.Get("Retry-After")
		}
		getProbes[digest] = probe
	}
	requestLogs, err = backendRegistryRequestLogs(ctx, environment, since)
	if err != nil {
		evidence["hubRequestLogAfterProbeError"] = err.Error()
	} else {
		evidence["hubRequestLogsAfterProbes"] = requestLogs
	}
	evidence["blobTasks"] = blobTasks
	evidence["blobIntents"] = blobIntents
	evidence["hubHeadResponses"] = headResponses
	evidence["hubGetProbes"] = getProbes
	return evidence
}

func blobIntentEvidenceForDigest(ctx context.Context, environment testkit.CaseContext, repository, digest string) (map[string]any, error) {
	query := url.Values{
		"authority":  {"github.com"},
		"repository": {repositoryPath(repository)},
		"digest":     {digest},
		"limit":      {"32"},
	}
	response, err := support.AdminRequest(ctx, environment, "GET", "/api/v1/blob-intents?"+query.Encode(), nil, nil)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		_ = response.Body.Close()
		return map[string]any{"status": response.StatusCode, "body": strings.TrimSpace(string(body))}, nil
	}
	var page map[string]any
	if _, err := support.DecodeJSON(response, 1<<20, &page); err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0)
	for _, raw := range testkit.Slice(page["items"]) {
		intent := testkit.Map(raw)
		items = append(items, summarizeBlobIntent(intent))
	}
	return map[string]any{"status": response.StatusCode, "items": items}, nil
}

func summarizeBlobIntent(intent map[string]any) map[string]any {
	fields := []string{
		"id", "identity", "round", "generation", "state", "demandRevision", "taskExists",
		"retryCount", "nextAttemptAt", "lastProgressAt", "createdAt", "updatedAt",
		"cancelRequested", "cancelPhase", "protectionSessionID", "errorClass", "errorSummary", "lastError", "readErrors",
	}
	summary := make(map[string]any, len(fields))
	for _, field := range fields {
		if value, ok := intent[field]; ok {
			summary[field] = value
		}
	}
	return summary
}

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

func backendRegistryRequestLogs(ctx context.Context, environment testkit.CaseContext, since time.Time) ([]string, error) {
	result, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", "deployment/"+environment.Fullname,
		"-c", "backend", "--since-time="+since.UTC().Format(time.RFC3339Nano), "--tail=300")
	if err != nil {
		return nil, err
	}
	var requests []string
	for _, line := range strings.Split(string(result.Stdout), "\n") {
		if (strings.Contains(line, `"msg":"registry request"`) || strings.Contains(line, `"msg":"registry blob delivery started"`)) && len(requests) < 60 {
			requests = append(requests, line)
		}
	}
	return requests, nil
}

func summarizeBlobTask(task map[string]any) map[string]any {
	fields := []string{
		"id", "kind", "status", "phase", "errorClass", "errorMessage", "retryable", "retryCount",
		"generation", "roundID", "attemptID", "nextAttemptAt", "lastProgressAt", "updatedAt",
		"waitReason", "waitResourceKey", "gcProtected", "protectionSessionID", "sessionIDs", "terminalCleanupComplete",
	}
	summary := make(map[string]any, len(fields)+1)
	for _, field := range fields {
		if value, ok := task[field]; ok {
			summary[field] = value
		}
	}
	if lastError, ok := task["lastError"]; ok {
		summary["lastError"] = lastError
	}
	return summary
}
