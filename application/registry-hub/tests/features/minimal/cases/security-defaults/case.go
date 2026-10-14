package securitydefaults

import (
	"context"
	"crypto/x509"
	"embed"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"os"
	"strings"
)

//go:embed case.json
var metadata embed.FS

type internalCredential struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "security-defaults")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	secret, data, err := support.GetSecret(ctx, environment.Namespace, environment.Fullname+"-registry-internal")
	if err != nil {
		return err
	}
	var credential internalCredential
	if err := json.Unmarshal(data["config.json"], &credential); err != nil || credential.Username == "" || credential.Password == "" {
		return fmt.Errorf("internal credential is invalid")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data["ca.crt"]) {
		return fmt.Errorf("internal CA is invalid")
	}
	service, err := testkit.KubectlGetObject(ctx, environment.Namespace, "service", environment.Fullname+"-registry")
	if err != nil {
		return err
	}
	clusterIP := testkit.String(testkit.Map(service.Raw["spec"])["clusterIP"])
	if clusterIP == "" || clusterIP == "None" {
		return fmt.Errorf("internal Registry Service has no ClusterIP")
	}
	policy, err := testkit.KubectlGetObject(ctx, environment.Namespace, "networkpolicy", environment.Fullname+"-registry")
	if err != nil {
		return err
	}
	if err := verifyPolicy(policy.Raw, environment); err != nil {
		return err
	}
	nodeName, err := registryNodeName(ctx, environment)
	if err != nil {
		return err
	}
	tlsEvidence, err := verifyInternalTLS(ctx, environment, clusterIP, nodeName)
	if err != nil {
		return err
	}
	probeEvidence, err := verifyNetworkPolicy(ctx, environment, clusterIP, nodeName)
	if err != nil {
		return err
	}

	registry := support.NewRegistryClient(environment)
	unsafeAuthority := "127.0.0.1:443"
	unsafeRepository := unsafeAuthority + "/team/" + environment.RunID
	response, err := registry.Get(ctx, unsafeRepository, "/manifests/missing", nil)
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusOK {
		response.Body.Close()
		return fmt.Errorf("SSRF authority was accepted")
	}
	response.Body.Close()
	taskID, status, retryable, err := waitForFailureTask(ctx, environment, unsafeAuthority, "team/"+environment.RunID)
	if err != nil {
		return err
	}
	if status != "failed" || retryable {
		return fmt.Errorf("SSRF task has unexpected state %s retryable=%t", status, retryable)
	}
	return support.WriteResult(environment, map[string]any{
		"internalSecretUID": secret.UID,
		"internalService":   clusterIP,
		"internalTLS":       tlsEvidence,
		"networkPolicy":     probeEvidence,
		"ssrfAuthority":     unsafeAuthority,
		"taskID":            taskID,
		"taskStatus":        status,
		"retryable":         retryable,
	})
}

func verifyInternalTLS(ctx context.Context, environment testkit.CaseContext, clusterIP, nodeName string) (map[string]string, error) {
	selector, err := backendSelector(ctx, environment)
	if err != nil {
		return nil, err
	}
	serviceName := environment.Fullname + "-registry"
	probeImage := os.Getenv("REGISTRY_HUB_TEST_PROBE_IMAGE")
	if probeImage == "" {
		probeImage = "container-registry-mirror.internal.home.geekcity.tech:32443/docker.io/curlimages/curl:8.10.1"
	}
	name := "rh-min-" + environment.RunID + "-tls"
	probeScript := fmt.Sprintf(`user=$(sed -n 's/.*"username":"\([^"]*\)".*/\1/p' /run/registry-ca/config.json)
pass=$(sed -n 's/.*"password":"\([^"]*\)".*/\1/p' /run/registry-ca/config.json)
request_status() {
  status=000
  attempt=0
  while [ "$attempt" -lt 15 ]; do
    status=$(curl --noproxy '*' --silent --show-error --connect-timeout 5 --max-time 8 "$@" --output /dev/null --write-out '%%{http_code}' 2>/dev/null || true)
    [ "$status" != "000" ] && break
    attempt=$((attempt + 1))
    sleep 1
  done
  printf '%%s' "$status"
}
good=$(request_status --cacert /run/registry-ca/ca.crt --resolve %s:5000:%s --user "$user:$pass" https://%s:5000/v2/)
printf 'tls-good:%%s\n' "$good"
wrong=$(request_status --cacert /run/registry-ca/ca.crt --resolve %s:5000:%s --user "$user:invalid-registry-hub-test" https://%s:5000/v2/)
printf 'tls-wrong:%%s\n' "$wrong"
if curl --noproxy '*' --silent --show-error --connect-timeout 5 --max-time 8 --cacert /dev/null --resolve %s:5000:%s --user "$user:$pass" --output /dev/null https://%s:5000/v2/ >/dev/null 2>&1; then printf 'tls-bad-ca:accepted\n'; else printf 'tls-bad-ca:failed\n'; fi
if curl --noproxy '*' --silent --show-error --connect-timeout 5 --max-time 8 --cacert /run/registry-ca/ca.crt --resolve wrong.registry.invalid:5000:%s --user "$user:$pass" --output /dev/null https://wrong.registry.invalid:5000/v2/ >/dev/null 2>&1; then printf 'tls-bad-name:accepted\n'; else printf 'tls-bad-name:failed\n'; fi
sleep 300`, serviceName, clusterIP, serviceName, serviceName, clusterIP, serviceName, serviceName, clusterIP, serviceName, clusterIP)
	if err := createProbe(ctx, environment, name, selector, probeImage, nodeName, probeScript, []map[string]any{{"key": "ca.crt", "path": "ca.crt"}, {"key": "config.json", "path": "config.json"}}); err != nil {
		return nil, err
	}
	defer deleteNetworkProbe(name, environment.Namespace)
	logs, err := waitProbeEvidence(ctx, environment, name, "tls-good:", "tls-wrong:", "tls-bad-ca:", "tls-bad-name:")
	if err != nil {
		return nil, err
	}
	evidence := make(map[string]string, 4)
	for _, line := range strings.Split(logs, "\n") {
		for _, key := range []string{"tls-good:", "tls-wrong:", "tls-bad-ca:", "tls-bad-name:"} {
			if strings.HasPrefix(line, key) {
				evidence[strings.TrimSuffix(key, ":")] = strings.TrimSpace(strings.TrimPrefix(line, key))
			}
		}
	}
	if evidence["tls-good"] != "200" || evidence["tls-wrong"] != "401" || evidence["tls-bad-ca"] != "failed" || evidence["tls-bad-name"] != "failed" {
		return nil, fmt.Errorf("internal TLS probe returned unexpected evidence: %v", evidence)
	}
	if err := verifyProbeNotReady(ctx, environment, name); err != nil {
		return nil, err
	}
	return evidence, nil
}

func verifyNetworkPolicy(ctx context.Context, environment testkit.CaseContext, clusterIP, nodeName string) (map[string]string, error) {
	selector, err := backendSelector(ctx, environment)
	if err != nil {
		return nil, err
	}
	serviceName := environment.Fullname + "-registry"
	probeImage := os.Getenv("REGISTRY_HUB_TEST_PROBE_IMAGE")
	if probeImage == "" {
		probeImage = "container-registry-mirror.internal.home.geekcity.tech:32443/docker.io/curlimages/curl:8.10.1"
	}
	probes := []struct {
		name   string
		labels map[string]string
		allow  bool
	}{
		{name: "rh-min-" + environment.RunID + "-np-deny", labels: map[string]string{"registry-hub.io/test-id": environment.RunID, "registry-hub.io/managed-by": "registry-hub-test-probe"}},
		{name: "rh-min-" + environment.RunID + "-np-allow", labels: selector, allow: true},
	}
	statuses := make(map[string]string, len(probes))
	for _, probe := range probes {
		if err := createNetworkProbe(ctx, environment, probe.name, probe.labels, probeImage, nodeName, serviceName, clusterIP); err != nil {
			return nil, err
		}
		defer deleteNetworkProbe(probe.name, environment.Namespace)
		status, err := waitProbeStatus(ctx, environment, probe.name)
		if err != nil {
			return nil, err
		}
		statuses[probe.name] = status
		allowedStatus := status == "200" || status == "401"
		if allowedStatus != probe.allow {
			return nil, fmt.Errorf("NetworkPolicy probe %s returned HTTP %s, allow=%t", probe.name, status, probe.allow)
		}
		if probe.allow {
			if err := verifyProbeNotReady(ctx, environment, probe.name); err != nil {
				return nil, err
			}
		}
	}
	return statuses, nil
}

func registryNodeName(ctx context.Context, environment testkit.CaseContext) (string, error) {
	endpoints, err := testkit.KubectlGet(ctx, environment.Namespace, "endpoints", environment.Fullname+"-registry")
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
	return "", fmt.Errorf("internal Registry endpoints do not expose a node name")
}

func backendSelector(ctx context.Context, environment testkit.CaseContext) (map[string]string, error) {
	deployment, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname)
	if err != nil {
		return nil, err
	}
	raw := testkit.Map(testkit.Map(deployment["spec"])["selector"])["matchLabels"]
	selector := make(map[string]string)
	for key, value := range testkit.Map(raw) {
		text := testkit.String(value)
		if text == "" {
			return nil, fmt.Errorf("backend Deployment selector contains an invalid label")
		}
		selector[key] = text
	}
	if len(selector) == 0 {
		return nil, fmt.Errorf("backend Deployment has no selector")
	}
	return selector, nil
}

func createNetworkProbe(ctx context.Context, environment testkit.CaseContext, name string, labels map[string]string, image, nodeName, serviceName, clusterIP string) error {
	probeScript := fmt.Sprintf("code=000; attempt=0; while [ \"$attempt\" -lt 15 ] && [ \"$code\" = 000 ]; do code=$(curl --noproxy '*' --silent --show-error --connect-timeout 5 --max-time 8 --cacert /run/registry-ca/ca.crt --resolve %s:5000:%s --output /dev/null --write-out '%%{http_code}' https://%s:5000/v2/ 2>/dev/null || true); [ \"$code\" = 000 ] && sleep 1; attempt=$((attempt + 1)); done; printf 'probe-status:%%s\\n' \"$code\"; sleep 300", serviceName, clusterIP, serviceName)
	return createProbe(ctx, environment, name, labels, image, nodeName, probeScript, []map[string]any{{"key": "ca.crt", "path": "ca.crt"}})
}
