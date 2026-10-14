package artifactroundtrip

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "artifact-roundtrip")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	registry := support.NewRegistryClient(environment)
	repository := support.Repository("artifact", environment.RunID)
	base, err := support.BuildImageFixture(ctx, registry, repository, "base", environment.RunID+"-base")
	if err != nil {
		return err
	}
	types := []struct {
		tag          string
		mediaType    string
		artifactType string
		layer        []byte
	}{
		{"helm", support.OCIManifestMediaType, "application/vnd.cncf.helm.config.v1+json", []byte("helm-chart-tar-gz:" + environment.RunID)},
		{"oras", support.OCIManifestMediaType, "application/vnd.example.oras.binary", []byte("oras-binary:" + environment.RunID)},
		{"sbom", support.OCIManifestMediaType, "application/spdx+json", []byte("{\"spdxVersion\":\"SPDX-2.3\"}")},
		{"signature", support.OCIManifestMediaType, "application/vnd.cncf.notary.signature", []byte("signature-payload:" + environment.RunID)},
		{"provenance", support.OCIManifestMediaType, "application/vnd.in-toto+json", []byte("{\"_type\":\"https://in-toto.io/Statement/v1\"}")},
		{"wasm", support.OCIManifestMediaType, "application/vnd.wasm.content.layer.v1+wasm", []byte("wasm-module:" + environment.RunID)},
	}
	digests := make([]string, 0, len(types)+3)
	manifests := make(map[string][]byte)
	for _, fixture := range types {
		content, digest, _, err := support.BuildArtifactManifest(ctx, registry, repository, fixture.tag, fixture.mediaType, fixture.artifactType, [][]byte{fixture.layer, []byte("shared-layer:" + environment.RunID)}, nil)
		if err != nil {
			return fmt.Errorf("artifact %s: %w", fixture.tag, err)
		}
		read, _, err := support.ReadManifest(ctx, registry, repository, fixture.tag, fixture.mediaType)
		if err != nil || !bytes.Equal(read, content) {
			return fmt.Errorf("artifact %s changed after read", fixture.tag)
		}
		digests = append(digests, digest)
		manifests[fixture.tag] = content
	}
	childAMD, childAMDDigest, _, err := support.BuildArtifactManifest(ctx, registry, repository, "child-amd64", support.OCIManifestMediaType, "application/vnd.example.child", [][]byte{[]byte("amd64:" + environment.RunID)}, nil)
	if err != nil {
		return err
	}
	childARM, childARMDigest, _, err := support.BuildArtifactManifest(ctx, registry, repository, "child-arm64", support.OCIManifestMediaType, "application/vnd.example.child", [][]byte{[]byte("arm64:" + environment.RunID)}, nil)
	if err != nil {
		return err
	}
	index := support.Manifest{SchemaVersion: 2, MediaType: support.OCIIndexMediaType, Manifests: []support.Descriptor{
		{MediaType: support.OCIManifestMediaType, Size: len(childAMD), Digest: childAMDDigest, Platform: map[string]any{"architecture": "amd64", "os": "linux"}},
		{MediaType: support.OCIManifestMediaType, Size: len(childARM), Digest: childARMDigest, Platform: map[string]any{"architecture": "arm64", "os": "linux"}},
	}}
	indexBytes, err := json.Marshal(index)
	if err != nil {
		return err
	}
	indexResponse, indexDigest, err := registry.PutManifest(ctx, repository, "multiarch", support.OCIIndexMediaType, indexBytes, nil)
	if indexResponse != nil {
		indexResponse.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("multiarch index PUT: %w", err)
	}
	indexRead, _, err := support.ReadManifest(ctx, registry, repository, "multiarch", support.OCIIndexMediaType)
	if err != nil || !bytes.Equal(indexRead, indexBytes) {
		return fmt.Errorf("multiarch index changed after read")
	}
	digests = append(digests, childAMDDigest, childARMDigest, indexDigest)

	referrer, referrerDigest, err := support.BuildReferrer(ctx, registry, repository, "base-referrer", "application/vnd.example.provenance", base.ManifestDigest)
	if err != nil {
		return err
	}
	referrerRead, _, err := support.ReadManifest(ctx, registry, repository, "base-referrer", support.OCIManifestMediaType)
	if err != nil || !bytes.Equal(referrerRead, referrer) {
		return fmt.Errorf("subject referrer changed after read")
	}
	digests = append(digests, base.ManifestDigest, referrerDigest)
	return support.WriteResult(environment, map[string]any{"repository": repository, "subjectDigest": base.ManifestDigest, "artifactDigests": digests, "manifestCount": len(manifests) + 3, "multiarchDigest": indexDigest, "referrerDigest": referrerDigest})
}
