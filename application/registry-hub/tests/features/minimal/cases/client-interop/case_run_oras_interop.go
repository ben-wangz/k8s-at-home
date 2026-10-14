package clientinterop

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"os"
	"path/filepath"
)

func runORASInterop(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, registryHost string) (map[string]any, error) {
	root := filepath.Join(environment.OutputDir, "oras")
	pulledRoot := filepath.Join(root, "pulled")
	attachedPullRoot := filepath.Join(root, "attached-pulled")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	for _, directory := range []string{pulledRoot, attachedPullRoot} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return nil, err
		}
	}
	repository := support.LocalRepository("oras-interop", environment.RunID)
	reference := registryHost + "/" + repository + ":" + environment.RunID
	artifactType := "application/vnd.example.registry-hub.binary.v1"
	artifactName := "oras-interop-" + environment.RunID + ".txt"
	artifactPath := filepath.Join(root, artifactName)
	artifact := []byte("registry-hub-oras-artifact:" + environment.RunID + "\n")
	if err := os.WriteFile(artifactPath, artifact, 0600); err != nil {
		return nil, err
	}
	if _, err := oras(ctx, root, "push", "--plain-http", "--artifact-type", artifactType, reference, artifactName+":text/plain"); err != nil {
		return nil, fmt.Errorf("push ORAS interoperability artifact: %w", err)
	}
	manifest, artifactDigest, err := readRegistryManifest(ctx, registry, repository, environment.RunID)
	if err != nil {
		return nil, err
	}
	if manifest.ArtifactType != artifactType || len(manifest.Layers) != 1 || manifest.Layers[0].Digest != support.Digest(artifact) || manifest.Layers[0].Size != len(artifact) {
		return nil, fmt.Errorf("ORAS artifact manifest does not preserve its artifact type and layer descriptor")
	}
	if _, err := oras(ctx, root, "pull", "--plain-http", "--output", "pulled", reference); err != nil {
		return nil, fmt.Errorf("pull ORAS interoperability artifact: %w", err)
	}
	pulledArtifact, err := os.ReadFile(filepath.Join(pulledRoot, artifactName))
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(pulledArtifact, artifact) {
		return nil, fmt.Errorf("ORAS artifact content changed during push and pull")
	}

	attachedName := "oras-interop-attached-" + environment.RunID + ".txt"
	attachedPath := filepath.Join(root, attachedName)
	attached := []byte("registry-hub-oras-associated-artifact:" + environment.RunID + "\n")
	if err := os.WriteFile(attachedPath, attached, 0600); err != nil {
		return nil, err
	}
	attachedType := "application/vnd.example.registry-hub.signature.v1"
	if _, err := oras(ctx, root, "attach", "--plain-http", "--artifact-type", attachedType, reference, attachedName+":text/plain"); err != nil {
		return nil, fmt.Errorf("attach ORAS referrer: %w", err)
	}
	discoveredResult, err := oras(ctx, root, "discover", "--plain-http", "--format", "json", reference)
	if err != nil {
		return nil, fmt.Errorf("discover ORAS referrer: %w", err)
	}
	var discovered struct {
		Manifests []struct {
			Digest       string `json:"digest"`
			ArtifactType string `json:"artifactType"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(discoveredResult.Stdout, &discovered); err != nil {
		return nil, fmt.Errorf("decode ORAS discover result: %w", err)
	}
	if len(discovered.Manifests) != 1 || discovered.Manifests[0].ArtifactType != attachedType || discovered.Manifests[0].Digest == "" {
		return nil, fmt.Errorf("ORAS discover did not return the associated artifact")
	}
	attachedDigest := discovered.Manifests[0].Digest
	attachedManifest, _, err := readRegistryManifest(ctx, registry, repository, attachedDigest)
	if err != nil {
		return nil, err
	}
	if attachedManifest.Subject == nil || attachedManifest.Subject.Digest != artifactDigest {
		return nil, fmt.Errorf("ORAS attached artifact does not reference the pushed subject")
	}
	attachedReference := registryHost + "/" + repository + "@" + attachedDigest
	if _, err := oras(ctx, root, "pull", "--plain-http", "--output", "attached-pulled", attachedReference); err != nil {
		return nil, fmt.Errorf("pull ORAS associated artifact: %w", err)
	}
	pulledAttached, err := os.ReadFile(filepath.Join(attachedPullRoot, attachedName))
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(pulledAttached, attached) {
		return nil, fmt.Errorf("ORAS associated artifact content changed during pull")
	}

	if _, err := oras(ctx, root, "manifest", "delete", "--plain-http", "--force", attachedReference); err != nil {
		return nil, fmt.Errorf("delete ORAS associated artifact: %w", err)
	}
	if err := requireManifestNotFound(ctx, registry, repository, attachedDigest); err != nil {
		return nil, err
	}
	discoveredResult, err = oras(ctx, root, "discover", "--plain-http", "--format", "json", reference)
	if err != nil {
		return nil, fmt.Errorf("verify ORAS referrer deletion: %w", err)
	}
	discovered.Manifests = nil
	if err := json.Unmarshal(discoveredResult.Stdout, &discovered); err != nil {
		return nil, fmt.Errorf("decode ORAS discover result after deletion: %w", err)
	}
	if len(discovered.Manifests) != 0 {
		return nil, fmt.Errorf("ORAS discover returned referrers after deletion")
	}
	if _, err := oras(ctx, root, "manifest", "delete", "--plain-http", "--force", reference); err != nil {
		return nil, fmt.Errorf("delete ORAS subject artifact: %w", err)
	}
	if err := requireManifestNotFound(ctx, registry, repository, artifactDigest); err != nil {
		return nil, err
	}
	return map[string]any{
		"orasVersion": "1.2.3", "orasRepository": repository, "orasArtifactDigest": artifactDigest,
		"orasArtifactType": artifactType, "orasAttachedDigest": attachedDigest, "orasAttachedType": attachedType,
		"orasPullPreservedContent": true, "orasDeleteVerified": true,
	}, nil
}

func readRegistryManifest(ctx context.Context, registry *support.RegistryClient, repository, reference string) (support.Manifest, string, error) {
	response, err := registry.Request(ctx, http.MethodGet, registry.Endpoint(repository, "/manifests/"+reference), nil, map[string]string{"Accept": support.OCIManifestMediaType})
	if err != nil {
		return support.Manifest{}, "", err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return support.Manifest{}, "", fmt.Errorf("manifest %s returned %s", reference, response.Status)
	}
	body, err := testkit.ReadBody(response, 4<<20)
	if err != nil {
		return support.Manifest{}, "", err
	}
	digest := support.Digest(body)
	if actual := response.Header.Get("Docker-Content-Digest"); actual != digest {
		return support.Manifest{}, "", fmt.Errorf("manifest %s digest %q does not match body %q", reference, actual, digest)
	}
	var manifest support.Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return support.Manifest{}, "", fmt.Errorf("decode manifest %s: %w", reference, err)
	}
	return manifest, digest, nil
}

func requireManifestNotFound(ctx context.Context, registry *support.RegistryClient, repository, digest string) error {
	response, err := registry.Request(ctx, http.MethodGet, registry.Endpoint(repository, "/manifests/"+digest), nil, map[string]string{"Accept": support.OCIManifestMediaType})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		return fmt.Errorf("deleted manifest %s returned %s", digest, response.Status)
	}
	return nil
}

func oras(ctx context.Context, directory string, arguments ...string) (testkit.CommandResult, error) {
	command := append([]string{"run", orasCommand}, arguments...)
	return testkit.CommandDir(ctx, directory, "go", command...)
}

func podman(ctx context.Context, storageRoot, runRoot string, arguments ...string) (testkit.CommandResult, error) {
	command := []string{"--root", storageRoot, "--runroot", runRoot, "--storage-driver=vfs"}
	command = append(command, arguments...)
	return testkit.Command(ctx, "podman", command...)
}

func imageRootfsArchive(runID string) ([]byte, error) {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	content := []byte("registry-hub-interoperability:" + runID + "\n")
	if err := writer.WriteHeader(&tar.Header{
		Name:     "registry-hub-interoperability.txt",
		Mode:     0600,
		Size:     int64(len(content)),
		Typeflag: tar.TypeReg,
		Format:   tar.FormatUSTAR,
	}); err != nil {
		return nil, err
	}
	if _, err := writer.Write(content); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
