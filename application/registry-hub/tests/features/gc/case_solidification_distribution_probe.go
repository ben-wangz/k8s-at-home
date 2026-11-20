package gc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

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
