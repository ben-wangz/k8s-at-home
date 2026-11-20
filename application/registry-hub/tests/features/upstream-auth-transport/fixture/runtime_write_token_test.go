package main

import (
	"archive/tar"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGCCancellationManifestOmitsExternalLayerURLs(t *testing.T) {
	fixture, err := buildGCCancellationManifest([]byte("{}"), []byte("layer"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Layers []struct {
			URLs []string `json:"urls"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(fixture.Manifest, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Layers) != 1 || len(manifest.Layers[0].URLs) != 0 {
		t.Fatalf("GC cancellation layer URLs = %#v, want none", manifest.Layers)
	}
}

func TestGCCancellationManifestCanBeReadByDigest(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	digest := "sha256:gc-cancellation-manifest"
	runtime := &fixtureRuntime{gcCancellationManifest: inlineManifestFixture{
		Manifest: manifest, ManifestDigest: digest,
	}}
	request := httptest.NewRequest(http.MethodGet, "/v2/registry-hub-test/fixture/manifests/"+digest, nil)
	response := httptest.NewRecorder()

	if !runtime.serveGitHubMinimalManifest(response, request, fixtureRequestState{host: "github.com", auth: "absent"}) {
		t.Fatal("digest request was not handled")
	}
	if response.Code != http.StatusOK || response.Header().Get("Docker-Content-Digest") != digest || !bytes.Equal(response.Body.Bytes(), manifest) {
		t.Fatalf("digest response = status %d, digest %q, body %q", response.Code, response.Header().Get("Docker-Content-Digest"), response.Body.Bytes())
	}
}

func TestGCCancellationLayerCanBePulledByDigest(t *testing.T) {
	content := []byte("gc-cancellation-missing-layer")
	layer, err := buildGCCancellationLayer(content)
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewReader(bytes.NewReader(layer))
	header, err := archive.Next()
	if err != nil || header.Name != "gc-cancellation.txt" || header.Typeflag != tar.TypeReg {
		t.Fatalf("cancellation layer tar header = %#v, err=%v", header, err)
	}
	decoded, err := io.ReadAll(archive)
	if err != nil || !bytes.Equal(decoded, content) {
		t.Fatalf("cancellation layer tar content = %q, err=%v", decoded, err)
	}
	digest := "sha256:" + hex.EncodeToString(sha256Bytes(layer))
	runtime := &fixtureRuntime{gcCancellationManifest: inlineManifestFixture{Layer: layer, LayerDigest: digest}}
	request := httptest.NewRequest(http.MethodGet, "/v2/registry-hub-test/fixture/blobs/"+digest, nil)
	response := httptest.NewRecorder()

	if !runtime.serveBlob(response, request, fixtureRequestState{host: "github.com", auth: "absent"}) {
		t.Fatal("digest request was not handled")
	}
	if response.Code != http.StatusOK || response.Header().Get("Docker-Content-Digest") != digest || !bytes.Equal(response.Body.Bytes(), layer) {
		t.Fatalf("blob response = status %d, digest %q, body %q", response.Code, response.Header().Get("Docker-Content-Digest"), response.Body.Bytes())
	}
}
