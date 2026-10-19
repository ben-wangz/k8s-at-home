package referrersfallback

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

//go:embed case.json
var metadata embed.FS

const (
	expectedImageRepository = "container-registry-mirror.internal.home.geekcity.tech:32443/docker.io/library/registry"
	expectedImageDigest     = "sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
	referrersProbeSuffix    = "-external-referrers-probe"
)

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "feature-referrers-fallback")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	if err := verifyRegistryImage(ctx, environment); err != nil {
		return err
	}
	registry := support.NewRegistryClient(environment)
	repository := support.LocalRepository(environment.CaseID, environment.RunID)
	subject, err := support.BuildImageFixture(ctx, registry, repository, "subject", environment.RunID)
	if err != nil {
		return err
	}
	tagsStatus, nativeStatus, err := ProbeNativeReferrers(ctx, environment, repository, subject.ManifestDigest)
	if err != nil {
		return err
	}
	if tagsStatus != http.StatusOK || nativeStatus != http.StatusNotFound {
		return fmt.Errorf("pinned Distribution native probe returned tags=%d referrers=%d, want 200/404", tagsStatus, nativeStatus)
	}
	upstreamBefore, err := upstreamRequests(ctx, environment)
	if err != nil {
		return err
	}
	traceStarted := time.Now().UTC().Add(-time.Second)
	referrer, referrerDigest, err := support.BuildReferrer(ctx, registry, repository, "signature", "application/vnd.example.signature", subject.ManifestDigest)
	if err != nil {
		return err
	}
	tag := fallbackTag(subject.ManifestDigest)
	descriptors, err := readIndex(ctx, registry, repository, "/referrers/"+subject.ManifestDigest, http.StatusOK)
	if err != nil {
		return err
	}
	if !hasDescriptor(descriptors, referrerDigest, "application/vnd.example.signature") {
		return fmt.Errorf("fallback Referrers response omitted the published artifact")
	}
	indexDigest, err := verifyFallbackTag(ctx, registry, repository, tag, referrerDigest)
	if err != nil {
		return err
	}
	protection, err := verifyFallbackProtection(ctx, registry, repository, tag, indexDigest, referrer, referrerDigest)
	if err != nil {
		return err
	}
	deleted, err := registry.Delete(ctx, repository, "/manifests/"+referrerDigest)
	if err != nil {
		return err
	}
	deleteStatus := deleted.StatusCode
	deleted.Body.Close()
	if deleteStatus != http.StatusAccepted {
		return fmt.Errorf("fallback referrer deletion returned %d, want 202", deleteStatus)
	}
	remaining, err := readIndex(ctx, registry, repository, "/referrers/"+subject.ManifestDigest, http.StatusOK)
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return fmt.Errorf("fallback Referrers index after delete contains %d descriptors", len(remaining))
	}
	if _, _, err := support.ReadManifest(ctx, registry, repository, subject.ManifestDigest, support.OCIManifestMediaType); err != nil {
		return fmt.Errorf("subject manifest was affected by fallback referrer deletion: %w", err)
	}
	upstreamAfter, err := upstreamRequests(ctx, environment)
	if err != nil {
		return err
	}
	if upstreamBefore != upstreamAfter {
		return fmt.Errorf("fallback Referrers operations triggered an upstream request")
	}
	fallbackReads, err := fallbackReferrerReads(ctx, environment)
	if err != nil {
		return err
	}
	if fallbackReads < 1 {
		return fmt.Errorf("fallback Referrers metric count = %v, want at least one read", fallbackReads)
	}
	apiTrace, err := verifyFallbackAPITrace(ctx, environment, repository, tag, referrerDigest, traceStarted)
	if err != nil {
		return err
	}
	return support.WriteResult(environment, map[string]any{
		"registryImage": expectedRegistryImageReference(),
		"repository":    repository, "subjectDigest": subject.ManifestDigest,
		"nativeTagsStatus": tagsStatus, "nativeReferrersStatus": nativeStatus,
		"fallbackTag": tag, "fallbackIndexDigest": indexDigest,
		"referrerDigest": referrerDigest, "referrerDeleteStatus": deleteStatus,
		"fallbackProtection": protection, "fallbackAPITrace": apiTrace,
		"remainingReferrers":     len(remaining),
		"fallbackReadMetric":     fallbackReads,
		"upstreamRequestsBefore": upstreamBefore, "upstreamRequestsAfter": upstreamAfter,
	})
}

func ProbeNativeReferrers(ctx context.Context, environment testkit.CaseContext, repository, subject string) (int, int, error) {
	serviceName := environment.Fullname + "-registry"
	service, err := testkit.KubectlGet(ctx, environment.Namespace, "service", serviceName)
	if err != nil {
		return 0, 0, err
	}
	clusterIP := testkit.String(testkit.Map(service["spec"])["clusterIP"])
	endpoints, err := testkit.KubectlGet(ctx, environment.Namespace, "endpoints", serviceName)
	if err != nil {
		return 0, 0, err
	}
	nodeName := endpointNode(endpoints)
	if clusterIP == "" || nodeName == "" {
		return 0, 0, fmt.Errorf("internal Registry Service has no ClusterIP or ready endpoint")
	}
	deployment, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname)
	if err != nil {
		return 0, 0, err
	}
	labels := testkit.Map(testkit.Map(testkit.Map(deployment["spec"])["selector"])["matchLabels"])
	if len(labels) == 0 {
		return 0, 0, fmt.Errorf("backend Deployment has no selector for the internal API probe")
	}
	image := "container-registry-mirror.internal.home.geekcity.tech:32443/docker.io/curlimages/curl:8.10.1"
	if configured := strings.TrimSpace(environmentVariable("REGISTRY_HUB_TEST_PROBE_IMAGE")); configured != "" {
		image = configured
	}
	name := environment.Release + referrersProbeSuffix
	probeLabels := make(map[string]any, len(labels)+3)
	for key, value := range labels {
		probeLabels[key] = value
	}
	probeLabels["registry-hub.io/test-id"] = environment.RunID
	probeLabels["registry-hub.io/instance"] = environment.Fullname
	probeLabels["registry-hub.io/managed-by"] = "registry-hub-test-fixture"
	referrersPath := "/v2/" + repository + "/referrers/" + url.PathEscape(subject)
	script := fmt.Sprintf(`user=$(sed -n 's/.*"username":"\([^"]*\)".*/\1/p' /run/registry-ca/config.json)
pass=$(sed -n 's/.*"password":"\([^"]*\)".*/\1/p' /run/registry-ca/config.json)
status() {
  code=000
  attempt=0
  while [ "$attempt" -lt 15 ] && [ "$code" = 000 ]; do
    code=$(curl --noproxy '*' --silent --connect-timeout 5 --max-time 8 --cacert /run/registry-ca/ca.crt --resolve %s:5000:%s --user "$user:$pass" --header 'Accept: application/vnd.oci.image.index.v1+json' --output /dev/null --write-out '%%{http_code}' "https://%s:5000$1" 2>/dev/null) || code=000
    [ "$code" = 000 ] && sleep 1
    attempt=$((attempt + 1))
  done
  printf '%%s' "$code"
}
tags=$(status '/v2/%s/tags/list')
referrers=$(status '%s')
printf 'native-probe:%%s:%%s\n' "$tags" "$referrers"
sleep 300`, serviceName, clusterIP, serviceName, repository, referrersPath)
	manifest := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": environment.Namespace, "labels": probeLabels},
		"spec": map[string]any{
			"automountServiceAccountToken": false, "nodeName": nodeName, "restartPolicy": "Never",
			"containers": []map[string]any{{
				"name": "probe", "image": image, "imagePullPolicy": "IfNotPresent",
				"command": []string{"sh", "-c"}, "args": []string{script},
				"readinessProbe":  map[string]any{"exec": map[string]any{"command": []string{"sh", "-c", "exit 1"}}, "periodSeconds": 1, "failureThreshold": 1},
				"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 1000, "allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []string{"ALL"}}},
				"volumeMounts":    []map[string]any{{"name": "registry-credentials", "mountPath": "/run/registry-ca", "readOnly": true}},
			}},
			"volumes": []map[string]any{{"name": "registry-credentials", "secret": map[string]any{"secretName": environment.Fullname + "-registry-internal", "items": []map[string]string{{"key": "ca.crt", "path": "ca.crt"}, {"key": "config.json", "path": "config.json"}}}}},
		},
	}
	content, err := json.Marshal(manifest)
	if err != nil {
		return 0, 0, err
	}
	if _, err := testkit.CommandInput(ctx, content, "kubectl", "apply", "-f", "-"); err != nil {
		return 0, 0, fmt.Errorf("create native Referrers probe: %w", err)
	}
	logs, err := waitProbe(ctx, environment, name)
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(logs, "\n") {
		if !strings.HasPrefix(line, "native-probe:") {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(line, "native-probe:"), ":")
		if len(parts) != 2 {
			break
		}
		tagsStatus, tagsErr := strconv.Atoi(parts[0])
		referrersStatus, referrersErr := strconv.Atoi(parts[1])
		if tagsErr == nil && referrersErr == nil {
			return tagsStatus, referrersStatus, nil
		}
	}
	return 0, 0, fmt.Errorf("native Referrers probe returned malformed evidence")
}

func endpointNode(endpoints map[string]any) string {
	for _, subset := range testkit.Slice(endpoints["subsets"]) {
		for _, address := range testkit.Slice(testkit.Map(subset)["addresses"]) {
			if name := testkit.String(testkit.Map(address)["nodeName"]); name != "" {
				return name
			}
		}
	}
	return ""
}
