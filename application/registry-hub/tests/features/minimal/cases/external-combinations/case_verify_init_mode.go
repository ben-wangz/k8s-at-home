package externalcombinations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"time"
)

func verifyInitMode(ctx context.Context, environment testkit.CaseContext, expected mode) error {
	selector := "app.kubernetes.io/instance=" + environment.Release + ",app.kubernetes.io/component=internal-initialization"
	jobs, err := testkit.KubectlList(ctx, environment.Namespace, "get", "jobs", "-l", selector)
	if err != nil {
		return err
	}
	if expected.NoInitJob {
		if len(jobs) != 0 {
			return fmt.Errorf("fully external configuration retained an initialization Job")
		}
		for _, resource := range []string{"role", "rolebinding", "serviceaccount"} {
			name := environment.Fullname + "-registry-init"
			if _, err := testkit.KubectlGet(ctx, environment.Namespace, resource, name); err == nil || !testkit.IsNotFound(err) {
				return fmt.Errorf("fully external configuration retained initialization %s", resource)
			}
		}
		for _, name := range []string{environment.Fullname, environment.Fullname + "-registry"} {
			deployment, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", name)
			if err != nil {
				return err
			}
			if len(testkit.Slice(deploymentPodSpec(deployment)["initContainers"])) != 0 {
				return fmt.Errorf("fully external configuration retained an initialization wait container")
			}
		}
		return nil
	}
	if len(jobs) != 1 || testkit.Int(testkit.Map(jobs[0].Raw["status"])["succeeded"]) != 1 {
		return fmt.Errorf("expected one completed initialization Job, found %d", len(jobs))
	}
	jobSpec := testkit.Map(testkit.Map(testkit.Map(jobs[0].Raw["spec"])["template"])["spec"])
	containers := testkit.Slice(jobSpec["containers"])
	if len(containers) != 1 {
		return fmt.Errorf("initialization Job has an unexpected container count")
	}
	values := make(map[string]string)
	for _, raw := range testkit.Slice(testkit.Map(containers[0])["env"]) {
		entry := testkit.Map(raw)
		values[testkit.String(entry["name"])] = testkit.String(entry["value"])
	}
	for name, auto := range map[string]bool{
		"REGISTRY_HUB_INIT_AUTH":        expected.InternalAuth,
		"REGISTRY_HUB_INIT_TLS":         expected.InternalTLS,
		"REGISTRY_HUB_INIT_HTTP_SECRET": expected.InternalHTTP,
		"REGISTRY_HUB_INIT_ADMIN_AUTH":  expected.AdminAuth,
	} {
		want := "false"
		if auto {
			want = "true"
		}
		if values[name] != want {
			return fmt.Errorf("initialization Job %s=%q, expected %q", name, values[name], want)
		}
	}
	return nil
}

func verifyServiceAccess(ctx context.Context, environment testkit.CaseContext, adminData map[string][]byte) error {
	client := testkit.NewHTTPClient(10*time.Second, nil)
	response, err := testkit.Request(ctx, client, http.MethodGet, environment.ClientURL+"/v2/", nil, nil, nil)
	if err != nil {
		return err
	}
	if err := testkit.ExpectStatus(response, http.StatusOK); err != nil {
		return err
	}
	var configuration support.AdminAuth
	if err := json.Unmarshal(adminData["config.json"], &configuration); err != nil || len(configuration.Basic) != 1 {
		return fmt.Errorf("generated management credential is invalid")
	}
	credential := configuration.Basic[0]
	response, err = testkit.Request(ctx, client, http.MethodGet, environment.AdminURL+"/docs", nil, nil, nil)
	if err != nil {
		return err
	}
	if err := testkit.ExpectStatus(response, http.StatusUnauthorized); err != nil {
		return err
	}
	response, err = testkit.Request(ctx, client, http.MethodGet, environment.AdminURL+"/docs", nil, nil, &testkit.BasicCredential{
		Username: credential.Username, Password: credential.Password,
	})
	if err != nil {
		return err
	}
	return testkit.ExpectStatus(response, http.StatusOK)
}

func fixtureName(environment testkit.CaseContext, suffix string) string {
	return environment.Release + "-external-" + suffix
}

func internalExternalValues(auth, htpasswd, tls, httpSecret string) map[string]any {
	return map[string]any{
		"backend": map[string]any{"distributionAuth": map[string]any{"existingSecret": auth}},
		"registry": map[string]any{
			"auth":       map[string]any{"htpasswdSecret": htpasswd},
			"tls":        map[string]any{"existingSecret": tls},
			"httpSecret": map[string]any{"existingSecret": httpSecret},
		},
	}
}

func mergeValues(left, right map[string]any) map[string]any {
	result := make(map[string]any, len(left)+len(right))
	for key, value := range left {
		result[key] = value
	}
	for key, value := range right {
		if leftValue, exists := result[key]; exists {
			leftMap := testkit.Map(leftValue)
			rightMap := testkit.Map(value)
			merged := make(map[string]any, len(leftMap)+len(rightMap))
			for child, childValue := range leftMap {
				merged[child] = childValue
			}
			for child, childValue := range rightMap {
				merged[child] = childValue
			}
			result[key] = merged
		} else {
			result[key] = value
		}
	}
	return result
}

func selectKeys(data map[string][]byte, keys ...string) map[string][]byte {
	selected := make(map[string][]byte, len(keys))
	for _, key := range keys {
		selected[key] = append([]byte(nil), data[key]...)
	}
	return selected
}

func fixtureSnapshots(fixtures []fixture) []secretSnapshot {
	result := make([]secretSnapshot, 0, len(fixtures))
	for _, current := range fixtures {
		result = append(result, current.Object)
	}
	return result
}

func hashBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func deploymentPodSpec(deployment map[string]any) map[string]any {
	spec := testkit.Map(deployment["spec"])
	template := testkit.Map(spec["template"])
	return testkit.Map(testkit.Map(template["spec"]))
}
