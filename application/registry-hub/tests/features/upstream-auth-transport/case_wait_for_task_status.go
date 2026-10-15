package upstreamauthtransport

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

func waitForTaskStatus(ctx context.Context, environment testkit.CaseContext, authority, repository, reference, expectedStatus string) (map[string]any, error) {
	query := url.Values{
		"authority":  {authority},
		"repository": {repository},
		"digest":     {reference},
		"limit":      {"100"},
	}
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	for {
		response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks?"+query.Encode(), nil, nil)
		if err != nil {
			return nil, err
		}
		var page map[string]any
		if _, err := support.DecodeJSON(response, 4<<20, &page); err != nil {
			return nil, err
		}
		for _, item := range testkit.Slice(page["items"]) {
			task := testkit.Map(item)
			if testkit.String(task["reference"]) != reference {
				continue
			}
			status := testkit.String(task["status"])
			if status == expectedStatus {
				return task, nil
			}
			if status == "failed" || status == "cancelled" {
				return nil, fmt.Errorf("task ended in %s (%s), want %s", status, testkit.String(task["errorClass"]), expectedStatus)
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("task did not reach %s", expectedStatus)
		case <-time.After(time.Second):
		}
	}
}

func verifyTokenRequestDeadline(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, repository string) (map[string]any, error) {
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "set", "env", "deployment/"+environment.Fullname, "REGISTRY_HUB_REQUEST_TIMEOUT_SECONDS=2"); err != nil {
		return nil, fmt.Errorf("set token request deadline: %w", err)
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "status", "deployment/"+environment.Fullname, "--timeout=180s"); err != nil {
		return nil, fmt.Errorf("backend did not restart with token request deadline: %w", err)
	}
	if err := WaitBackendServiceEndpoint(ctx, environment); err != nil {
		return nil, err
	}
	const reference = "token-deadline"
	started := time.Now()
	response, err := registry.Get(ctx, repository, "/manifests/"+reference, map[string]string{"Accept": support.OCIManifestMediaType})
	if err != nil {
		return nil, fmt.Errorf("token request deadline call failed before returning a response: %w", err)
	}
	envelope, decodeErr := support.DecodeError(response)
	status := response.StatusCode
	retryAfter := response.Header.Get("Retry-After")
	response.Body.Close()
	if decodeErr != nil {
		return nil, decodeErr
	}
	errors := testkit.Slice(envelope["errors"])
	if status != http.StatusGatewayTimeout || retryAfter != "5" || len(errors) != 1 || testkit.String(testkit.Map(errors[0])["code"]) != "UNAVAILABLE" {
		return nil, fmt.Errorf("token request deadline response is invalid: status=%d retry-after=%q errors=%v", status, retryAfter, envelope["errors"])
	}
	elapsed := time.Since(started)
	if elapsed < time.Second || elapsed > 10*time.Second {
		return nil, fmt.Errorf("token request deadline elapsed %s, want approximately 2 seconds", elapsed)
	}
	query := url.Values{
		"authority":  {"docker.io"},
		"repository": {"library/registry-hub-test"},
		"digest":     {reference},
		"limit":      {"100"},
	}
	taskResponse, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks?"+query.Encode(), nil, nil)
	if err != nil {
		return nil, err
	}
	var page map[string]any
	if _, err := support.DecodeJSON(taskResponse, 4<<20, &page); err != nil {
		return nil, err
	}
	items := testkit.Slice(page["items"])
	if len(items) != 1 {
		return nil, fmt.Errorf("token request deadline task count = %d, want one", len(items))
	}
	task := testkit.Map(items[0])
	if testkit.String(task["status"]) != "failed" || testkit.String(task["rootDigest"]) != "" || task["rootPublished"] == true {
		return nil, fmt.Errorf("token request deadline task state is invalid: status=%q root=%q published=%v", testkit.String(task["status"]), testkit.String(task["rootDigest"]), task["rootPublished"])
	}
	logs, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", "pod/"+environment.Release+proxySuffix, "-c", "proxy", "--tail=50")
	if err != nil {
		return nil, err
	}
	logText := string(logs.Stdout)
	registryMarker := "method=GET host=registry-1.docker.io path=/v2/library/registry-hub-test/manifests/" + reference + " auth=basic-matched"
	tokenMarker := "method=GET host=auth.docker.io path=/token auth=basic-matched scope=repository:library/registry-hub-test:pull service=token-deadline"
	if strings.Count(logText, registryMarker) != 1 || strings.Count(logText, tokenMarker) != 1 {
		return nil, fmt.Errorf("token request deadline fixture trace is invalid: registry=%d token=%d", strings.Count(logText, registryMarker), strings.Count(logText, tokenMarker))
	}
	return map[string]any{
		"status":          status,
		"elapsedMillis":   elapsed.Milliseconds(),
		"retryAfter":      retryAfter,
		"tokenRequests":   strings.Count(logText, tokenMarker),
		"taskStatus":      testkit.String(task["status"]),
		"taskErrorClass":  testkit.String(task["errorClass"]),
		"rootUnpublished": task["rootPublished"] != true,
	}, nil
}

func validateSecret(data map[string][]byte, release, namespace string) (string, string, string, error) {
	if len(data) != 6 {
		return "", "", "", fmt.Errorf("upstream config Secret must contain three config values and three TLS fixture values")
	}
	if string(data["UPSTREAM_TLS_CERT"]) != string(data["UPSTREAM_TLS_CA_CERT"]) {
		return "", "", "", fmt.Errorf("upstream TLS certificate and CA differ")
	}
	if _, err := tls.X509KeyPair(data["UPSTREAM_TLS_CERT"], data["UPSTREAM_TLS_KEY"]); err != nil {
		return "", "", "", fmt.Errorf("upstream TLS fixture certificate is invalid")
	}
	var auth map[string]authCredential
	if err := json.Unmarshal(data["REGISTRY_HUB_UPSTREAM_AUTH_CONFIG"], &auth); err != nil {
		return "", "", "", fmt.Errorf("upstream auth configuration is invalid")
	}
	credential, ok := auth["docker.io"]
	bearer, bearerOK := auth["ghcr.io"]
	if len(auth) != 2 || !ok || credential.Type != "basic" || credential.Username != "registry-hub-test" || len(credential.Password) < 32 ||
		!bearerOK || bearer.Type != "bearer" || len(bearer.Token) < 32 {
		return "", "", "", fmt.Errorf("upstream auth configuration does not match the test contract")
	}
	var transports map[string]transportRule
	if err := json.Unmarshal(data["REGISTRY_HUB_UPSTREAM_TRANSPORT_CONFIG"], &transports); err != nil {
		return "", "", "", fmt.Errorf("upstream transport configuration is invalid")
	}
	caFile := "/etc/registry-hub/upstream-tls/ca.crt"
	if len(transports) != 5 || !validHTTPRule(transports["docker.io"]) || !validHTTPAndTLSRule(transports["github.com"], caFile) || !validHTTPRule(transports["ghcr.io"]) ||
		!validTLSRule(transports["tls.registry-hub.test"], caFile) || !validTLSRule(transports["tls-untrusted.registry-hub.test"], "") {
		return "", "", "", fmt.Errorf("upstream transport configuration does not match the test contract")
	}
	var proxy proxyConfiguration
	if err := json.Unmarshal(data["REGISTRY_HUB_PROXY_CONFIG"], &proxy); err != nil {
		return "", "", "", fmt.Errorf("upstream proxy configuration is invalid")
	}
	proxyURL := "http://" + release + proxySuffix + "." + namespace + ".svc.cluster.local:8080"
	if proxy.Default != "" || len(proxy.Authorities) != 5 || proxy.Authorities["docker.io"] != proxyURL ||
		proxy.Authorities["github.com"] != proxyURL || proxy.Authorities["ghcr.io"] != proxyURL ||
		proxy.Authorities["tls.registry-hub.test"] != proxyURL || proxy.Authorities["tls-untrusted.registry-hub.test"] != proxyURL {
		return "", "", "", fmt.Errorf("upstream proxy configuration does not match the test contract")
	}
	return credential.Password, bearer.Token, secretFingerprint(data), nil
}

func validHTTPRule(rule transportRule) bool {
	return rule.AllowHTTP && len(rule.Ports) == 1 && rule.Ports[0] == 80
}

func validHTTPAndTLSRule(rule transportRule, caFile string) bool {
	return rule.AllowHTTP && rule.CAFile == caFile && len(rule.Ports) == 2 && rule.Ports[0] == 80 && rule.Ports[1] == 443
}

func validTLSRule(rule transportRule, caFile string) bool {
	return !rule.AllowHTTP && rule.CAFile == caFile && len(rule.Ports) == 1 && rule.Ports[0] == 443
}

func secretFingerprint(data map[string][]byte) string {
	hash := sha256.New()
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		_, _ = hash.Write([]byte(key))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(data[key])
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func validateManifestResponse(body []byte, headers http.Header) error {
	if headers.Get("Content-Type") == "" || !strings.Contains(headers.Get("Content-Type"), support.OCIManifestMediaType) {
		return fmt.Errorf("response Content-Type is not OCI image manifest")
	}
	if actual := headers.Get("Docker-Content-Digest"); actual != "" && actual != support.Digest(body) {
		return fmt.Errorf("response digest header does not match the manifest bytes")
	}
	var manifest support.Manifest
	if err := json.Unmarshal(body, &manifest); err != nil || manifest.SchemaVersion != 2 || manifest.MediaType != support.OCIManifestMediaType {
		return fmt.Errorf("response body is not a valid OCI manifest")
	}
	return nil
}

func createProxyFixture(ctx context.Context, environment testkit.CaseContext, secretName string) error {
	fixtureSources, err := FixtureSources()
	if err != nil {
		return err
	}
	return CreateProxyFixture(ctx, environment, secretName, fixtureSources, nil)
}

func FixtureSources() (map[string]string, error) {
	entries, err := assets.ReadDir("fixture")
	if err != nil {
		return nil, err
	}
	sources := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		path := "fixture/" + entry.Name()
		content, err := assets.ReadFile(path)
		if err != nil {
			return nil, err
		}
		sources[entry.Name()] = string(content)
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("upstream fixture has no Go sources")
	}
	return sources, nil
}
