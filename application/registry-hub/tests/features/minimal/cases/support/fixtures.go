package support

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

type ImageFixture struct {
	Repository     string
	Tag            string
	Manifest       []byte
	ManifestType   string
	ManifestDigest string
	Config         []byte
	ConfigDigest   string
	Layers         map[string][]byte
}

func Repository(caseID, runID string) string {
	return "test.invalid/registry-hub/" + caseID + "/" + runID
}

func LocalRepository(caseID, runID string) string {
	return "local-" + caseID + "-" + runID
}

func TarLayer(name, content string) ([]byte, error) {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	data := []byte(content)
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatUSTAR}); err != nil {
		return nil, err
	}
	if _, err := writer.Write(data); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func BuildImageFixture(ctx context.Context, registry *RegistryClient, repository, tag, runID string) (ImageFixture, error) {
	layer, err := TarLayer("fixture.txt", "registry-hub-minimal:"+runID+":"+repository)
	if err != nil {
		return ImageFixture{}, err
	}
	layerDigest, err := registry.PutBlob(ctx, repository, layer)
	if err != nil {
		return ImageFixture{}, fmt.Errorf("push layer: %w", err)
	}
	config := []byte("{\"architecture\":\"amd64\",\"config\":{},\"os\":\"linux\",\"rootfs\":{\"type\":\"layers\",\"diff_ids\":[\"" + layerDigest + "\"]}}")
	configDigest, err := registry.PutBlob(ctx, repository, config)
	if err != nil {
		return ImageFixture{}, fmt.Errorf("push config: %w", err)
	}
	manifest := Manifest{
		SchemaVersion: 2,
		MediaType:     OCIManifestMediaType,
		Config:        &Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Size: len(config), Digest: configDigest},
		Layers:        []Descriptor{{MediaType: "application/vnd.oci.image.layer.v1.tar", Size: len(layer), Digest: layerDigest}},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return ImageFixture{}, err
	}
	response, manifestDigest, err := registry.PutManifest(ctx, repository, tag, OCIManifestMediaType, manifestBytes, nil)
	if response != nil {
		response.Body.Close()
	}
	if err != nil {
		return ImageFixture{}, fmt.Errorf("base manifest PUT: %w", err)
	}
	return ImageFixture{Repository: repository, Tag: tag, Manifest: manifestBytes, ManifestType: OCIManifestMediaType, ManifestDigest: manifestDigest, Config: config, ConfigDigest: configDigest, Layers: map[string][]byte{layerDigest: layer}}, nil
}

func BuildArtifactManifest(ctx context.Context, registry *RegistryClient, repository, reference, mediaType, artifactType string, layers [][]byte, subject *Descriptor) ([]byte, string, []string, error) {
	descriptors := make([]Descriptor, 0, len(layers))
	for index, content := range layers {
		digest, err := registry.PutBlob(ctx, repository, content)
		if err != nil {
			return nil, "", nil, fmt.Errorf("push artifact layer %d: %w", index, err)
		}
		descriptors = append(descriptors, Descriptor{MediaType: "application/vnd.example.layer.v1", Size: len(content), Digest: digest, Annotations: map[string]string{"org.opencontainers.image.title": fmt.Sprintf("layer-%d", index)}})
	}
	config := []byte("{\"created\":\"1970-01-01T00:00:00Z\",\"architecture\":\"unknown\",\"os\":\"unknown\"}")
	configDigest, err := registry.PutBlob(ctx, repository, config)
	if err != nil {
		return nil, "", nil, fmt.Errorf("push artifact config: %w", err)
	}
	manifest := Manifest{SchemaVersion: 2, MediaType: mediaType, ArtifactType: artifactType, Config: &Descriptor{MediaType: "application/vnd.example.config.v1+json", Size: len(config), Digest: configDigest}, Layers: descriptors, Subject: subject, Annotations: map[string]string{"org.opencontainers.image.created": "1970-01-01T00:00:00Z", "test.registry-hub/run": "minimal"}}
	content, err := json.Marshal(manifest)
	if err != nil {
		return nil, "", nil, err
	}
	response, digest, err := registry.PutManifest(ctx, repository, reference, mediaType, content, nil)
	if response != nil {
		subjectHeader := response.Header.Get("OCI-Subject")
		response.Body.Close()
		if err == nil && subject != nil && subjectHeader != subject.Digest {
			return nil, "", nil, fmt.Errorf("manifest %s PUT returned OCI-Subject %q, want %q", reference, subjectHeader, subject.Digest)
		}
	}
	if err != nil {
		return nil, "", nil, fmt.Errorf("manifest %s PUT: %w", reference, err)
	}
	return content, digest, append([]string{configDigest}, descriptorDigests(descriptors)...), nil
}

func descriptorDigests(descriptors []Descriptor) []string {
	result := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		result = append(result, descriptor.Digest)
	}
	return result
}

func BuildReferrer(ctx context.Context, registry *RegistryClient, repository, reference, artifactType, subjectDigest string) ([]byte, string, error) {
	subjectBody, subjectHeaders, err := ReadManifest(ctx, registry, repository, subjectDigest, OCIManifestMediaType)
	if err != nil {
		return nil, "", fmt.Errorf("read referrer subject: %w", err)
	}
	subjectMediaType := subjectHeaders.Get("Content-Type")
	if index := strings.IndexByte(subjectMediaType, ';'); index >= 0 {
		subjectMediaType = subjectMediaType[:index]
	}
	if subjectMediaType == "" {
		subjectMediaType = OCIManifestMediaType
	}
	subject := Descriptor{MediaType: subjectMediaType, Digest: subjectDigest, Size: len(subjectBody)}
	content, digest, _, err := BuildArtifactManifest(ctx, registry, repository, reference, OCIManifestMediaType, artifactType, [][]byte{[]byte("referrer:" + reference)}, &subject)
	return content, digest, err
}

func InlineDescriptor(content []byte, mediaType string) Descriptor {
	return Descriptor{MediaType: mediaType, Size: len(content), Digest: Digest(content), Data: base64.StdEncoding.EncodeToString(content)}
}

func ReadManifest(ctx context.Context, registry *RegistryClient, repository, reference, accepts string) ([]byte, http.Header, error) {
	headers := map[string]string{}
	if accepts != "" {
		headers["Accept"] = accepts
	}
	response, err := registry.Get(ctx, repository, "/manifests/"+reference, headers)
	if err != nil {
		return nil, nil, err
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		return nil, response.Header, fmt.Errorf("manifest GET returned %s", response.Status)
	}
	body, err := testkit.ReadBody(response, 16<<20)
	return body, response.Header, err
}

func ReadBlob(ctx context.Context, registry *RegistryClient, repository, digest string) ([]byte, error) {
	response, err := registry.Get(ctx, repository, "/blobs/"+digest, nil)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		return nil, fmt.Errorf("blob GET returned %s", response.Status)
	}
	return testkit.ReadBody(response, 100<<20)
}

func DecodeError(response *http.Response) (map[string]any, error) {
	var document map[string]any
	if response == nil {
		return nil, fmt.Errorf("nil response")
	}
	_, err := DecodeJSON(response, 2<<20, &document)
	return document, err
}

func DetailString(result testkit.CaseResultFile, key string) (string, error) {
	value, ok := result.Details[key].(string)
	if !ok || value == "" {
		return "", fmt.Errorf("dependency %s has no %s", result.CaseID, key)
	}
	return value, nil
}
