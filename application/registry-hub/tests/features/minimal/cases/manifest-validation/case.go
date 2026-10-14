package manifestvalidation

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "manifest-validation")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	registry := support.NewRegistryClient(environment)
	repository := support.Repository("manifest", environment.RunID)
	fixture, err := support.BuildImageFixture(ctx, registry, repository, "oci", environment.RunID)
	if err != nil {
		return err
	}
	raw, headers, err := support.ReadManifest(ctx, registry, repository, fixture.ManifestDigest, support.OCIManifestMediaType)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, fixture.Manifest) || headers.Get("Docker-Content-Digest") != fixture.ManifestDigest {
		return fmt.Errorf("digest manifest returned altered bytes or digest")
	}
	layerDigest := ""
	for digest := range fixture.Layers {
		layerDigest = digest
	}
	dockerManifest := support.Manifest{SchemaVersion: 2, MediaType: support.DockerManifestMedia, Config: &support.Descriptor{MediaType: "application/vnd.docker.container.image.v1+json", Size: len(fixture.Config), Digest: fixture.ConfigDigest}, Layers: []support.Descriptor{{MediaType: "application/vnd.docker.image.rootfs.diff.tar.gzip", Size: len(fixture.Layers[layerDigest]), Digest: layerDigest}}}
	dockerBytes, err := json.Marshal(dockerManifest)
	if err != nil {
		return err
	}
	if err := putAndReadManifest(ctx, registry, repository, "docker", support.DockerManifestMedia, dockerBytes); err != nil {
		return err
	}
	urlManifest := support.Manifest{
		SchemaVersion: 2,
		MediaType:     support.OCIManifestMediaType,
		Config:        &support.Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Size: len(fixture.Config), Digest: fixture.ConfigDigest},
		Layers: []support.Descriptor{{
			MediaType: "application/vnd.oci.image.layer.v1.tar",
			Size:      len(fixture.Layers[layerDigest]),
			Digest:    layerDigest,
			URLs:      []string{"http://127.0.0.1:1/should-not-be-fetched"},
		}},
		Annotations: map[string]string{"test": environment.RunID},
	}
	urlManifestBytes, err := json.Marshal(urlManifest)
	if err != nil {
		return err
	}
	if err := putAndReadManifest(ctx, registry, repository, "urls", support.OCIManifestMediaType, urlManifestBytes); err != nil {
		return fmt.Errorf("manifest with descriptor.urls: %w", err)
	}

	invalidReference := "invalid-digest"
	invalid := []byte("{\"schemaVersion\":2,\"mediaType\":\"application/vnd.oci.image.manifest.v1+json\",\"config\":{\"mediaType\":\"application/vnd.oci.image.config.v1+json\",\"digest\":\"sha256:bad\",\"size\":1}}")
	response, err := registry.Request(ctx, http.MethodPut, registry.Endpoint(repository, "/manifests/"+invalidReference), bytes.NewReader(invalid), map[string]string{"Content-Type": support.OCIManifestMediaType})
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusCreated {
		response.Body.Close()
		return fmt.Errorf("invalid manifest was published")
	}
	response.Body.Close()
	missing, err := registry.Get(ctx, repository, "/manifests/"+invalidReference, nil)
	if err != nil {
		return err
	}
	if missing.StatusCode == http.StatusOK {
		missing.Body.Close()
		return fmt.Errorf("invalid manifest reference remained visible")
	}
	missing.Body.Close()

	inlineContent := []byte("inline-fixture")
	descriptor := support.InlineDescriptor(inlineContent, "application/vnd.example.inline")
	inlineManifest := support.Manifest{SchemaVersion: 2, MediaType: support.OCIManifestMediaType, Config: &descriptor, Annotations: map[string]string{"test": environment.RunID}}
	inlineBytes, err := json.Marshal(inlineManifest)
	if err != nil {
		return err
	}
	inlineResponse, _, inlineErr := registry.PutManifest(ctx, repository, "inline", support.OCIManifestMediaType, inlineBytes, url.Values{})
	if inlineResponse != nil {
		inlineResponse.Body.Close()
	}
	if inlineErr != nil {
		return fmt.Errorf("valid inline descriptor was rejected: %w", inlineErr)
	}
	inlineRead, _, err := support.ReadManifest(ctx, registry, repository, "inline", support.OCIManifestMediaType)
	if err != nil || !bytes.Equal(inlineRead, inlineBytes) {
		return fmt.Errorf("valid inline descriptor manifest was not preserved")
	}
	inlineBlob, err := support.ReadBlob(ctx, registry, repository, descriptor.Digest)
	if err != nil || !bytes.Equal(inlineBlob, inlineContent) {
		return fmt.Errorf("valid inline descriptor bytes were not published")
	}

	invalidContent := []byte("rejected-inline-fixture")
	invalidDescriptor := support.InlineDescriptor(invalidContent, "application/vnd.example.inline")
	invalidDescriptor.Data = "invalid-base64"
	invalidInlineManifest := support.Manifest{SchemaVersion: 2, MediaType: support.OCIManifestMediaType, Config: &invalidDescriptor, Annotations: map[string]string{"test": environment.RunID}}
	invalidInlineBytes, err := json.Marshal(invalidInlineManifest)
	if err != nil {
		return err
	}
	invalidRepository := support.LocalRepository("manifest-invalid-inline", environment.RunID)
	tasksBefore, err := taskIDs(ctx, environment)
	if err != nil {
		return err
	}
	invalidResponse, err := registry.Request(ctx, http.MethodPut, registry.Endpoint(invalidRepository, "/manifests/invalid-inline-data"), bytes.NewReader(invalidInlineBytes), map[string]string{"Content-Type": support.OCIManifestMediaType})
	if err != nil {
		return err
	}
	invalidInlineStatus := invalidResponse.StatusCode
	if invalidInlineStatus != http.StatusBadRequest {
		invalidResponse.Body.Close()
		return fmt.Errorf("invalid descriptor.data returned %s", invalidResponse.Status)
	}
	invalidResponse.Body.Close()
	missingManifest, err := registry.Get(ctx, invalidRepository, "/manifests/invalid-inline-data", nil)
	if err != nil {
		return err
	}
	if missingManifest.StatusCode != http.StatusNotFound {
		missingManifest.Body.Close()
		return fmt.Errorf("invalid descriptor.data manifest returned %s after rejection", missingManifest.Status)
	}
	missingManifest.Body.Close()
	missingBlob, err := registry.Get(ctx, invalidRepository, "/blobs/"+invalidDescriptor.Digest, nil)
	if err != nil {
		return err
	}
	if missingBlob.StatusCode != http.StatusNotFound {
		missingBlob.Body.Close()
		return fmt.Errorf("invalid descriptor.data blob returned %s after rejection", missingBlob.Status)
	}
	missingBlob.Body.Close()
	tasksAfter, err := taskIDs(ctx, environment)
	if err != nil {
		return err
	}
	for id := range tasksAfter {
		if _, existed := tasksBefore[id]; !existed {
			return fmt.Errorf("invalid descriptor.data created task %s", id)
		}
	}
	return support.WriteResult(environment, map[string]any{"repository": repository, "manifestDigest": fixture.ManifestDigest, "configDigest": fixture.ConfigDigest, "layerDigest": layerDigest, "dockerManifest": support.Digest(dockerBytes), "urlManifestDigest": support.Digest(urlManifestBytes), "urlDescriptorPreserved": true, "inlineAccepted": true, "inlineBlobDigest": descriptor.Digest, "invalidInlineStatus": invalidInlineStatus, "invalidInlineManifestAbsent": true, "invalidInlineBlobAbsent": true, "invalidInlineTaskCountBefore": len(tasksBefore), "invalidInlineTaskCountAfter": len(tasksAfter)})
}

func taskIDs(ctx context.Context, environment testkit.CaseContext) (map[string]struct{}, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks?limit=1000", nil, nil)
	if err != nil {
		return nil, err
	}
	var page map[string]any
	if _, err := support.DecodeJSON(response, 4<<20, &page); err != nil {
		return nil, err
	}
	result := make(map[string]struct{})
	for _, item := range testkit.Slice(page["items"]) {
		if id := testkit.String(testkit.Map(item)["id"]); id != "" {
			result[id] = struct{}{}
		}
	}
	return result, nil
}

func putAndReadManifest(ctx context.Context, registry *support.RegistryClient, repository, reference, mediaType string, content []byte) error {
	response, digest, err := registry.PutManifest(ctx, repository, reference, mediaType, content, nil)
	if response != nil {
		response.Body.Close()
	}
	if err != nil {
		return err
	}
	raw, _, err := support.ReadManifest(ctx, registry, repository, reference, mediaType)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, content) || support.Digest(raw) != digest {
		return fmt.Errorf("manifest %s was rewritten", reference)
	}
	return nil
}
