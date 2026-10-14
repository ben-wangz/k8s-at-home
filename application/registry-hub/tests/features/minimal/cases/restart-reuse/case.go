package restartreuse

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"strings"
	"time"
)

//go:embed case.json
var metadata embed.FS

type snapshot struct {
	UID         string
	Hashes      map[string]string
	StateSHA256 string
}

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "restart-reuse")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	fixture, exists := environment.DependencyResults["registry-roundtrip"]
	if !exists || fixture.Status != "passed" {
		return fmt.Errorf("OCI roundtrip dependency result is unavailable")
	}
	repository, err := detailString(fixture.Details, "repository")
	if err != nil {
		return err
	}
	manifestDigest, err := detailString(fixture.Details, "manifestDigest")
	if err != nil {
		return err
	}
	configDigest, err := detailString(fixture.Details, "configDigest")
	if err != nil {
		return err
	}
	layerDigest, err := detailString(fixture.Details, "layerDigest")
	if err != nil {
		return err
	}
	before, err := captureSecrets(ctx, environment)
	if err != nil {
		return err
	}
	firstRender, err := renderChart(ctx, environment)
	if err != nil {
		return err
	}
	removedJobUID, err := deleteInitializationJob(ctx, environment)
	if err != nil {
		return err
	}
	if _, err := testkit.Command(ctx, "helm", "upgrade", "--install", environment.Release, environment.ChartDir,
		"--namespace", environment.Namespace,
		"--values", environment.BaseValues,
		"--values", environment.ValuesFile,
		"--wait", "--wait-for-jobs", "--timeout", "15m"); err != nil {
		return err
	}
	secondRender, err := renderChart(ctx, environment)
	if err != nil {
		return err
	}
	if firstRender != secondRender {
		return fmt.Errorf("repeated Helm rendering produced different manifests")
	}
	recreatedJobUID, err := verifyRecreatedInitializationJob(ctx, environment, removedJobUID)
	if err != nil {
		return err
	}
	for _, deployment := range []string{environment.Fullname, environment.Fullname + "-registry"} {
		if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "restart", "deployment/"+deployment); err != nil {
			return err
		}
		if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "status", "deployment/"+deployment, "--timeout=5m"); err != nil {
			return err
		}
	}
	after, err := captureSecrets(ctx, environment)
	if err != nil {
		return err
	}
	if !equalSnapshots(before, after) {
		return fmt.Errorf("generated credentials changed after Helm upgrade or Pod restart")
	}
	response, err := testkit.Request(ctx, testkit.NewHTTPClient(10*time.Second, nil), http.MethodGet, environment.ClientURL+"/v2/", nil, nil, nil)
	if err != nil {
		return err
	}
	if err := testkit.ExpectStatus(response, http.StatusOK); err != nil {
		return err
	}
	if err := verifyStoredContent(ctx, environment, repository, manifestDigest, configDigest, layerDigest); err != nil {
		return err
	}
	reinstalledJobUID, err := uninstallAndReinstall(ctx, environment, before)
	if err != nil {
		return err
	}
	return support.WriteResult(environment, map[string]any{
		"renderSHA256":         firstRender,
		"initializationJobUID": map[string]string{"removed": removedJobUID, "recreated": recreatedJobUID, "reinstalled": reinstalledJobUID},
		"objectIdentities": map[string]string{
			"internalSecret": before["internal-secret"].UID,
			"adminSecret":    before["admin-secret"].UID,
			"internalRecord": before["internal-record"].UID,
			"adminRecord":    before["admin-record"].UID,
		},
		"secretKeySHA256": map[string]any{
			"internal": before["internal-secret"].Hashes,
			"admin":    before["admin-secret"].Hashes,
		},
		"recordDataSHA256": map[string]string{
			"internal": before["internal-record"].StateSHA256,
			"admin":    before["admin-record"].StateSHA256,
		},
		"releaseUninstallRetention": true,
	})
}

func uninstallAndReinstall(ctx context.Context, environment testkit.CaseContext, expected map[string]snapshot) (string, error) {
	if _, err := testkit.Command(ctx, "helm", "uninstall", environment.Release, "--namespace", environment.Namespace, "--wait", "--timeout", "5m"); err != nil {
		return "", err
	}
	afterUninstall, err := captureSecrets(ctx, environment)
	if err != nil {
		return "", err
	}
	if !equalSnapshots(expected, afterUninstall) {
		return "", fmt.Errorf("release uninstall removed or changed generated credentials or initialization records")
	}
	if _, err := testkit.Command(ctx, "helm", "upgrade", "--install", environment.Release, environment.ChartDir,
		"--namespace", environment.Namespace,
		"--values", environment.BaseValues,
		"--values", environment.ValuesFile,
		"--wait", "--wait-for-jobs", "--timeout", "15m"); err != nil {
		return "", err
	}
	for _, deployment := range []string{environment.Fullname, environment.Fullname + "-registry"} {
		if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "status", "deployment/"+deployment, "--timeout=5m"); err != nil {
			return "", err
		}
	}
	jobs, err := testkit.KubectlList(ctx, environment.Namespace, "get", "jobs", "-l", "app.kubernetes.io/instance="+environment.Release+",app.kubernetes.io/component=internal-initialization")
	if err != nil {
		return "", err
	}
	if len(jobs) != 1 || jobs[0].UID == "" || testkit.Int(testkit.Map(jobs[0].Raw["status"])["succeeded"]) != 1 {
		return "", fmt.Errorf("reinstalled release did not complete its initialization Job")
	}
	afterInstall, err := captureSecrets(ctx, environment)
	if err != nil {
		return "", err
	}
	if !equalSnapshots(expected, afterInstall) {
		return "", fmt.Errorf("reinstall changed generated credentials or initialization records")
	}
	return jobs[0].UID, nil
}

func detailString(details map[string]any, key string) (string, error) {
	value, ok := details[key].(string)
	if !ok || value == "" {
		return "", fmt.Errorf("OCI roundtrip dependency has no %s fixture", key)
	}
	return value, nil
}

func verifyStoredContent(ctx context.Context, environment testkit.CaseContext, repository, manifestDigest, configDigest, layerDigest string) error {
	base := environment.ClientURL + "/v2/" + repository
	client := testkit.NewHTTPClient(20*time.Second, nil)
	for _, object := range []struct {
		path   string
		digest string
	}{
		{path: "/manifests/" + manifestDigest, digest: manifestDigest},
		{path: "/blobs/" + configDigest, digest: configDigest},
		{path: "/blobs/" + layerDigest, digest: layerDigest},
	} {
		headers := map[string]string(nil)
		if strings.HasPrefix(object.path, "/manifests/") {
			headers = map[string]string{"Accept": "application/vnd.oci.image.manifest.v1+json"}
		}
		response, err := testkit.Request(ctx, client, http.MethodGet, base+object.path, nil, headers, nil)
		if err != nil {
			return err
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return fmt.Errorf("stored OCI content %s returned %s", object.path, response.Status)
		}
		body, err := testkit.ReadBody(response, 1<<20)
		if err != nil {
			return err
		}
		if contentDigest(body) != object.digest {
			return fmt.Errorf("stored OCI content %s no longer matches its digest", object.path)
		}
	}
	return nil
}

func contentDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}
