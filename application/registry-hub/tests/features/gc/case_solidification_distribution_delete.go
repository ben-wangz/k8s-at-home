package gc

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

type distributionCredential struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func directDistributionDeleteBlob(ctx context.Context, environment testkit.CaseContext, repository, digest string) (evidence map[string]any, err error) {
	serviceName := environment.Fullname + "-registry"
	service, err := testkit.KubectlGet(ctx, environment.Namespace, "service", serviceName)
	if err != nil {
		return nil, err
	}
	spec := testkit.Map(service["spec"])
	clusterIP := testkit.String(spec["clusterIP"])
	if net.ParseIP(clusterIP) == nil || !serviceHasRegistryPort(spec) {
		return nil, fmt.Errorf("Distribution Service endpoint is invalid")
	}
	nodeName, err := distributionNodeName(ctx, environment, serviceName)
	if err != nil {
		return nil, err
	}

	secret, secretData, err := support.GetSecret(ctx, environment.Namespace, environment.Fullname+"-registry-internal")
	if err != nil {
		return nil, err
	}
	if secret.Labels["registry-hub.io/instance"] != environment.Fullname || secret.Labels["registry-hub.io/managed-by"] != "internal-initializer" {
		return nil, fmt.Errorf("internal Distribution Secret ownership is invalid")
	}
	var credential distributionCredential
	if err := json.Unmarshal(secretData["config.json"], &credential); err != nil || credential.Username != "registry-hub-backend" || credential.Password == "" || len(secretData["ca.crt"]) == 0 {
		return nil, fmt.Errorf("internal Distribution credentials or CA are invalid")
	}
	deployment, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname)
	if err != nil {
		return nil, err
	}
	selector := testkit.Map(testkit.Map(testkit.Map(deployment["spec"])["selector"])["matchLabels"])
	if len(selector) == 0 {
		return nil, fmt.Errorf("backend Deployment has no selector for the Distribution probe")
	}
	internalRepository := internalDistributionRepository(repository)
	probeName := environment.Release + "-dist-delete"
	probeImage := strings.TrimSpace(os.Getenv("REGISTRY_HUB_TEST_PROBE_IMAGE"))
	if probeImage == "" {
		probeImage = "container-registry-mirror.internal.home.geekcity.tech:32443/docker.io/curlimages/curl:8.10.1"
	}
	manifest, err := directDistributionProbeManifest(environment, selector, probeName, probeImage, serviceName, clusterIP, nodeName, internalRepository, digest)
	if err != nil {
		return nil, err
	}
	if _, err := testkit.CommandInput(ctx, manifest, "kubectl", "apply", "-f", "-"); err != nil {
		return nil, fmt.Errorf("create direct Distribution probe Pod: %w", err)
	}
	probeCreated := true
	defer func() {
		if !probeCreated {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		_, cleanupErr := testkit.Command(cleanupCtx, "kubectl", "-n", environment.Namespace, "delete", "pod", probeName, "--ignore-not-found", "--wait=true", "--timeout=75s")
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("delete direct Distribution probe Pod: %w", cleanupErr))
			return
		}
		if evidence != nil {
			evidence["probePodCleaned"] = true
		}
	}()

	statuses, probeOutput, err := waitForDirectDistributionProbe(ctx, environment, probeName)
	if err != nil {
		return nil, directDistributionProbeFailure(ctx, environment, internalRepository, digest, nil, probeOutput, err)
	}
	if statuses[0] != "200/0" || statuses[1] != "200/0" || statuses[2] != "202/0" || statuses[3] != "404/0" {
		return nil, directDistributionProbeFailure(ctx, environment, internalRepository, digest, statuses, probeOutput, nil)
	}
	evidence = map[string]any{
		"service": serviceName, "probePod": probeName,
		"distributionRepository": internalRepository,
		"distributionV2Result":   statuses[0], "readBeforeDeleteResult": statuses[1],
		"directDeleteResult": statuses[2], "readAfterDeleteResult": statuses[3],
		"apiPath": "Distribution Service /v2 blob API",
	}
	return evidence, nil
}

func directDistributionProbeFailure(ctx context.Context, environment testkit.CaseContext, repository, digest string, statuses []string, probeOutput string, cause error) error {
	requests, logErr := directDistributionBlobRequestEvidence(ctx, environment, digest)
	evidence := map[string]any{
		"distributionRepository": repository,
		"blobDigest":             digest,
		"probeResults":           statuses,
		"probeOutput":            strings.TrimSpace(probeOutput),
		"distributionRequests":   requests,
	}
	if logErr != nil {
		evidence["distributionLogError"] = logErr.Error()
	}
	encoded, _ := json.Marshal(evidence)
	if cause != nil {
		return fmt.Errorf("direct Distribution probe failed: %w; evidence: %s", cause, encoded)
	}
	return fmt.Errorf("direct Distribution /v2/ and blob GET/DELETE/GET results = %v, want [200/0 200/0 202/0 404/0]; evidence: %s", statuses, encoded)
}

func directDistributionBlobRequestEvidence(ctx context.Context, environment testkit.CaseContext, digest string) ([]string, error) {
	lines, err := distributionLogs(ctx, environment, time.Time{})
	if err != nil {
		return nil, err
	}
	requests := make([]string, 0, 8)
	for _, line := range lines {
		if !strings.Contains(line, `http.request.uri=`) || !strings.Contains(line, digest) {
			continue
		}
		if len(line) > 4096 {
			line = line[:4096] + "[truncated]"
		}
		requests = append(requests, line)
		if len(requests) == 50 {
			break
		}
	}
	return requests, nil
}

func internalDistributionRepository(repository string) string {
	authority, remainder, found := strings.Cut(repository, "/")
	if !found || !strings.ContainsAny(authority, ".:") && !strings.HasPrefix(authority, "e") && authority != "localhost" {
		return repository
	}
	var encoded strings.Builder
	encoded.WriteByte('e')
	for _, character := range []byte(authority) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			encoded.WriteByte(character)
			continue
		}
		encoded.WriteByte('_')
		encoded.WriteString(hex.EncodeToString([]byte{character}))
	}
	return encoded.String() + "/" + remainder
}

func serviceHasRegistryPort(spec map[string]any) bool {
	for _, raw := range testkit.Slice(spec["ports"]) {
		port := testkit.Map(raw)
		if testkit.Int(port["port"]) == 5000 && testkit.String(port["name"]) == "https-registry" {
			return true
		}
	}
	return false
}

func distributionNodeName(ctx context.Context, environment testkit.CaseContext, serviceName string) (string, error) {
	endpoints, err := testkit.KubectlGet(ctx, environment.Namespace, "endpoints", serviceName)
	if err != nil {
		return "", err
	}
	for _, subset := range testkit.Slice(endpoints["subsets"]) {
		for _, address := range testkit.Slice(testkit.Map(subset)["addresses"]) {
			if nodeName := testkit.String(testkit.Map(address)["nodeName"]); nodeName != "" {
				return nodeName, nil
			}
		}
	}
	return "", fmt.Errorf("Distribution endpoints do not expose a node name")
}

func directDistributionProbeManifest(environment testkit.CaseContext, selector map[string]any, name, image, serviceName, clusterIP, nodeName, repository, digest string) ([]byte, error) {
	labels := make(map[string]any, len(selector)+3)
	for key, value := range selector {
		labels[key] = value
	}
	labels["registry-hub.io/test-id"] = environment.RunID
	labels["registry-hub.io/instance"] = environment.Fullname
	labels["registry-hub.io/managed-by"] = "registry-hub-test-fixture"
	baseURL := "https://" + serviceName + ":5000"
	blobURL := baseURL + "/v2/" + strings.Trim(repository, "/") + "/blobs/" + digest
	script := fmt.Sprintf(`user=$(sed -n 's/.*"username":"\([^"]*\)".*/\1/p' /run/registry-ca/config.json)
pass=$(sed -n 's/.*"password":"\([^"]*\)".*/\1/p' /run/registry-ca/config.json)
request() {
  output=$(curl --noproxy '*' --silent --connect-timeout 5 --max-time 15 --cacert /run/registry-ca/ca.crt --resolve %s:5000:%s --user "$user:$pass" --output /dev/null --write-out ':%%{http_code}' --request "$1" "$2" 2>/dev/null)
  exit_code=$?
  status=${output##*:}
  [ -n "$status" ] || status=000
  [ "$exit_code" -eq 0 ] || status=000
  printf '%%s/%%s' "$status" "$exit_code"
}
version=000/7
attempt=0
while [ "$attempt" -lt 15 ] && [ "${version%%/*}" = 000 ]; do
  version=$(request GET %s/v2/)
  [ "${version%%/*}" = 000 ] && sleep 1
  attempt=$((attempt + 1))
done
if [ "${version%%/*}" = 000 ]; then
  before=SKIPPED
  deleted=SKIPPED
  after=SKIPPED
else
  before=$(request GET %s)
  if [ "$before" = '200/0' ]; then
    deleted=$(request DELETE %s)
    after=$(request GET %s)
  else
    deleted=SKIPPED
    after=SKIPPED
  fi
fi
printf 'direct-distribution:%%s:%%s:%%s:%%s\n' "$version" "$before" "$deleted" "$after"
sleep 300`, shellQuote(serviceName), shellQuote(clusterIP), shellQuote(baseURL), shellQuote(blobURL), shellQuote(blobURL), shellQuote(blobURL))
	pod := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": environment.Namespace, "labels": labels},
		"spec": map[string]any{
			"automountServiceAccountToken": false, "restartPolicy": "Never",
			"nodeName": nodeName,
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
	return json.Marshal(pod)
}

func waitForDirectDistributionProbe(ctx context.Context, environment testkit.CaseContext, name string) ([]string, string, error) {
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	for {
		pod, err := testkit.KubectlGet(ctx, environment.Namespace, "pod", name)
		if err == nil {
			phase := testkit.String(testkit.Map(pod["status"])["phase"])
			if phase == "Running" {
				logs, logErr := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", name, "-c", "probe", "--tail=20")
				if logErr == nil {
					output := string(logs.Stdout)
					for _, line := range strings.Split(output, "\n") {
						if strings.HasPrefix(line, "direct-distribution:") {
							parts := strings.Split(strings.TrimPrefix(line, "direct-distribution:"), ":")
							if len(parts) == 4 {
								return parts, output, nil
							}
						}
					}
				}
			}
			if phase == "Failed" || phase == "Succeeded" {
				return nil, "", fmt.Errorf("direct Distribution probe Pod ended in %s before producing evidence", phase)
			}
		}
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-deadline.C:
			return nil, "", fmt.Errorf("direct Distribution probe did not produce GET/DELETE evidence")
		case <-time.After(time.Second):
		}
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
