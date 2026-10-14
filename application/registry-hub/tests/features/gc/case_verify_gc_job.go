package gc

import (
	"bytes"
	"context"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"strings"
)

func verifyGCJob(ctx context.Context, environment testkit.CaseContext, started, finished map[string]any) (map[string]any, error) {
	jobName := testkit.String(finished["jobName"])
	if jobName == "" || len(testkit.String(started["id"])) < 12 || testkit.String(finished["id"]) != testkit.String(started["id"]) {
		return nil, fmt.Errorf("GC run identity changed while executing")
	}
	job, err := testkit.KubectlGet(ctx, environment.Namespace, "job", jobName)
	if err != nil {
		return nil, err
	}
	metadata := testkit.Map(job["metadata"])
	annotations := testkit.Map(metadata["annotations"])
	spec := testkit.Map(job["spec"])
	status := testkit.Map(job["status"])
	template := testkit.Map(spec["template"])
	podSpec := testkit.Map(template["spec"])
	containers := testkit.Slice(podSpec["containers"])
	if len(containers) != 1 {
		return nil, fmt.Errorf("GC Job must have exactly one container")
	}
	container := testkit.Map(containers[0])
	image := testkit.String(container["image"])
	labels := testkit.Map(metadata["labels"])
	registryDeployment, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname+"-registry")
	if err != nil {
		return nil, err
	}
	registryImage := deploymentContainerImage(registryDeployment, "registry")
	configDigest := testkit.String(finished["configDigest"])
	jobUID := testkit.String(metadata["uid"])
	if configDigest == "" || jobUID == "" || jobUID != testkit.String(finished["jobUID"]) ||
		testkit.String(labels["registry-hub.io/gc-id"]) != testkit.String(started["id"])[:12] ||
		testkit.String(annotations["registry-hub.io/config-digest"]) != configDigest ||
		testkit.Int(status["succeeded"]) != 1 || testkit.Int(status["failed"]) != 0 ||
		testkit.Int(spec["backoffLimit"]) != 0 || testkit.Int(spec["completions"]) != 1 || testkit.Int(spec["parallelism"]) != 1 ||
		testkit.Int(spec["ttlSecondsAfterFinished"]) != 604800 ||
		testkit.String(podSpec["restartPolicy"]) != "Never" || image == "" || image != registryImage ||
		testkit.String(podSpec["serviceAccountName"]) != environment.Fullname ||
		podSpec["automountServiceAccountToken"] != false ||
		!jobUsesRegistryConfig(podSpec, environment.Fullname+"-registry-config") ||
		!jobUsesRegistryStorage(podSpec, environment.Fullname+"-registry") ||
		!jobMountsVolume(container, "registry-config", "/etc/distribution", true) ||
		!jobMountsVolume(container, "registry-storage", "/var/lib/registry", false) {
		return nil, fmt.Errorf("GC Job identity, policy, image, config or storage does not match the running Registry")
	}
	pvc, err := testkit.KubectlGet(ctx, environment.Namespace, "persistentvolumeclaim", environment.Fullname+"-registry")
	if err != nil {
		return nil, fmt.Errorf("GC Job registry PVC is not bound: %w", err)
	}
	if testkit.String(testkit.Map(pvc["status"])["phase"]) != "Bound" {
		return nil, fmt.Errorf("GC Job registry PVC is not bound")
	}
	return map[string]any{
		"name": jobName, "uid": jobUID, "image": image, "configDigest": configDigest,
		"registryPVC": environment.Fullname + "-registry",
	}, nil
}

func deploymentContainerImage(deployment map[string]any, name string) string {
	template := testkit.Map(testkit.Map(deployment["spec"])["template"])
	for _, raw := range testkit.Slice(testkit.Map(template["spec"])["containers"]) {
		container := testkit.Map(raw)
		if testkit.String(container["name"]) == name {
			return testkit.String(container["image"])
		}
	}
	return ""
}

func jobUsesRegistryConfig(podSpec map[string]any, expectedConfigMap string) bool {
	for _, raw := range testkit.Slice(podSpec["volumes"]) {
		volume := testkit.Map(raw)
		if testkit.String(testkit.Map(volume["configMap"])["name"]) == expectedConfigMap {
			return true
		}
	}
	return false
}

func jobUsesRegistryStorage(podSpec map[string]any, expectedClaim string) bool {
	for _, raw := range testkit.Slice(podSpec["volumes"]) {
		volume := testkit.Map(raw)
		claim := testkit.String(testkit.Map(volume["persistentVolumeClaim"])["claimName"])
		if claim == expectedClaim {
			return true
		}
	}
	return false
}

func jobMountsVolume(container map[string]any, expectedName, expectedPath string, expectedReadOnly bool) bool {
	for _, raw := range testkit.Slice(container["volumeMounts"]) {
		mount := testkit.Map(raw)
		readOnly, configured := mount["readOnly"].(bool)
		if !configured {
			readOnly = false
		}
		if testkit.String(mount["name"]) == expectedName && testkit.String(mount["mountPath"]) == expectedPath && readOnly == expectedReadOnly {
			return true
		}
	}
	return false
}

func verifyGCContents(ctx context.Context, registry *support.RegistryClient, repository string, retained support.ImageFixture, orphanDigest string) error {
	response, err := registry.Get(ctx, repository, "/blobs/"+orphanDigest, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusNotFound {
		response.Body.Close()
		return fmt.Errorf("GC retained unreferenced blob: GET returned %s", response.Status)
	}
	response.Body.Close()
	manifest, headers, err := support.ReadManifest(ctx, registry, repository, retained.Tag, support.OCIManifestMediaType)
	if err != nil {
		return fmt.Errorf("GC damaged the retained manifest: %w", err)
	}
	if !bytes.Equal(manifest, retained.Manifest) || !strings.EqualFold(headers.Get("Docker-Content-Digest"), retained.ManifestDigest) {
		return fmt.Errorf("GC changed the retained tag or manifest bytes")
	}
	config, err := support.ReadBlob(ctx, registry, repository, retained.ConfigDigest)
	if err != nil {
		return fmt.Errorf("GC removed retained config %s: %w", retained.ConfigDigest, err)
	}
	if !bytes.Equal(config, retained.Config) {
		return fmt.Errorf("GC changed retained config %s bytes", retained.ConfigDigest)
	}
	for digest, expected := range retained.Layers {
		actual, err := support.ReadBlob(ctx, registry, repository, digest)
		if err != nil {
			return fmt.Errorf("GC damaged retained layer %s: %w", digest, err)
		}
		if !bytes.Equal(actual, expected) {
			return fmt.Errorf("GC changed retained layer %s bytes", digest)
		}
	}
	return nil
}

func firstLayerDigest(fixture support.ImageFixture) string {
	for digest := range fixture.Layers {
		return digest
	}
	return ""
}
