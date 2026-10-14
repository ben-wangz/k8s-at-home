package clientinterop

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
)

//go:embed case.json
var metadata embed.FS

const orasCommand = "oras.land/oras/cmd/oras@v1.2.3"

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "client-interop")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	podmanRoot, err := os.MkdirTemp("", "rhi-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(podmanRoot)
	storageRoot := filepath.Join(podmanRoot, "s")
	runRoot := filepath.Join(podmanRoot, "r")

	rootfsArchive, err := imageRootfsArchive(environment.RunID)
	if err != nil {
		return err
	}
	archivePath := filepath.Join(environment.OutputDir, "client-image-rootfs.tar")
	if err := os.WriteFile(archivePath, rootfsArchive, 0600); err != nil {
		return err
	}
	defer os.Remove(archivePath)

	localImage := "localhost/registry-hub-interop:" + environment.RunID
	if _, err := podman(ctx, storageRoot, runRoot, "import", archivePath, localImage); err != nil {
		return fmt.Errorf("import Podman interoperability fixture: %w", err)
	}
	registryURL, err := url.Parse(environment.ClientURL)
	if err != nil || registryURL.Scheme != "http" || registryURL.Host == "" {
		return fmt.Errorf("client Registry Service URL is invalid")
	}
	repositoryRoot := support.LocalRepository("client-interop", environment.RunID)
	remoteImage := registryURL.Host + "/" + repositoryRoot + ":" + environment.RunID
	if _, err := podman(ctx, storageRoot, runRoot, "push", "--tls-verify=false", localImage, "docker://"+remoteImage); err != nil {
		return fmt.Errorf("push Podman interoperability fixture: %w", err)
	}
	if _, err := podman(ctx, storageRoot, runRoot, "pull", "--tls-verify=false", "docker://"+remoteImage); err != nil {
		return fmt.Errorf("pull Podman interoperability fixture: %w", err)
	}
	registry := support.NewRegistryClient(environment)
	manifestResponse, err := registry.Request(ctx, http.MethodGet, registry.Endpoint(repositoryRoot, "/manifests/"+environment.RunID), nil, map[string]string{
		"Accept": "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json",
	})
	if err != nil {
		return err
	}
	if manifestResponse.StatusCode != http.StatusOK {
		manifestResponse.Body.Close()
		return fmt.Errorf("Podman-pushed manifest returned %s", manifestResponse.Status)
	}
	manifest, err := testkit.ReadBody(manifestResponse, 4<<20)
	if err != nil {
		return err
	}
	manifestDigest := support.Digest(manifest)
	if actual := manifestResponse.Header.Get("Docker-Content-Digest"); actual != manifestDigest {
		return fmt.Errorf("Podman-pushed manifest digest %q does not match body %q", actual, manifestDigest)
	}

	chartDir := filepath.Join(environment.OutputDir, "helm-chart")
	packageDir := filepath.Join(environment.OutputDir, "helm-package")
	pulledDir := filepath.Join(environment.OutputDir, "helm-pulled")
	for _, directory := range []string{chartDir, packageDir, pulledDir} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return err
		}
	}
	chartMetadata := []byte("apiVersion: v2\nappVersion: \"1\"\nname: registry-hub-interop\ntype: application\nversion: 0.1.0\n")
	chartValues := []byte("runID: " + environment.RunID + "\n")
	if err := os.WriteFile(filepath.Join(chartDir, "Chart.yaml"), chartMetadata, 0600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(chartDir, "values.yaml"), chartValues, 0600); err != nil {
		return err
	}
	if _, err := testkit.Command(ctx, "helm", "package", chartDir, "--destination", packageDir); err != nil {
		return fmt.Errorf("package Helm interoperability fixture: %w", err)
	}
	packagePath := filepath.Join(packageDir, "registry-hub-interop-0.1.0.tgz")
	chartRepository := "oci://" + registryURL.Host
	if _, err := testkit.Command(ctx, "helm", "push", "--plain-http", packagePath, chartRepository); err != nil {
		return fmt.Errorf("push Helm interoperability fixture: %w", err)
	}
	pullRepository := chartRepository + "/registry-hub-interop"
	if _, err := testkit.Command(ctx, "helm", "pull", "--plain-http", pullRepository, "--version", "0.1.0", "--untar", "--untardir", pulledDir); err != nil {
		return fmt.Errorf("pull Helm interoperability fixture: %w", err)
	}
	pulledMetadata, err := os.ReadFile(filepath.Join(pulledDir, "registry-hub-interop", "Chart.yaml"))
	if err != nil {
		return err
	}
	pulledValues, err := os.ReadFile(filepath.Join(pulledDir, "registry-hub-interop", "values.yaml"))
	if err != nil {
		return err
	}
	if !bytes.Equal(pulledMetadata, chartMetadata) || !bytes.Equal(pulledValues, chartValues) {
		return fmt.Errorf("Helm chart content changed during OCI push and pull")
	}
	orasDetails, err := runORASInterop(ctx, environment, registry, registryURL.Host)
	if err != nil {
		return err
	}
	details := map[string]any{
		"podmanImage":           localImage,
		"registryRepository":    repositoryRoot,
		"manifestDigest":        manifestDigest,
		"helmRepository":        pullRepository,
		"helmChartVersion":      "0.1.0",
		"podmanStorageIsolated": true,
	}
	for key, value := range orasDetails {
		details[key] = value
	}
	return support.WriteResult(environment, details)
}
