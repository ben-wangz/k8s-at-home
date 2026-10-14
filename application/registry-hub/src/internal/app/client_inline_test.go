package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/distribution"
	"registry-hub/internal/route"
	"strconv"
	"testing"
)

func TestEnsureEmbeddedBlobUsesStandardUploadAPI(t *testing.T) {
	data := []byte("inline descriptor blob")
	digest := artifact.CalculateSHA256(data)
	var stored []byte
	var postCount, putCount int
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodHead && request.URL.Path == "/v2/test/repository/blobs/"+digest:
			if stored == nil {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
			writer.Header().Set("Docker-Content-Digest", digest)
			writer.WriteHeader(http.StatusOK)
		case request.Method == http.MethodPost && request.URL.Path == "/v2/test/repository/blobs/uploads/":
			postCount++
			writer.Header().Set("Location", "/v2/test/repository/blobs/uploads/session")
			writer.WriteHeader(http.StatusAccepted)
		case request.Method == http.MethodPut && request.URL.Path == "/v2/test/repository/blobs/uploads/session":
			putCount++
			if request.URL.Query().Get("digest") != digest {
				t.Errorf("upload digest query = %q, want %q", request.URL.Query().Get("digest"), digest)
			}
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read upload body: %v", err)
			}
			stored = body
			writer.Header().Set("Docker-Content-Digest", digest)
			writer.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected Distribution request: %s %s", request.Method, request.URL.RequestURI())
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	caPath := filepath.Join(t.TempDir(), "distribution-ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := distribution.New(config.Config{DistributionURL: base, DistributionCA: caPath})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()

	service := &Service{registry: registry}
	descriptor := artifact.Descriptor{Digest: digest, Size: int64(len(data)), Embedded: data}
	beforeWriteCalls := 0
	beforeWrite := func() error {
		beforeWriteCalls++
		return nil
	}
	if err := service.ensureEmbeddedBlob(context.Background(), "test/repository", descriptor, beforeWrite); err != nil {
		t.Fatalf("ensure inline blob: %v", err)
	}
	if string(stored) != string(data) || postCount != 1 || putCount != 1 || beforeWriteCalls != 1 {
		t.Fatalf("uploaded=%q POSTs=%d PUTs=%d beforeWrite=%d", stored, postCount, putCount, beforeWriteCalls)
	}
	if err := service.ensureEmbeddedBlob(context.Background(), "test/repository", descriptor, func() error {
		t.Fatal("beforeWrite ran for an existing blob")
		return nil
	}); err != nil {
		t.Fatalf("verify existing inline blob: %v", err)
	}
	if postCount != 1 || putCount != 1 {
		t.Fatalf("existing blob triggered an upload: POSTs=%d PUTs=%d", postCount, putCount)
	}
}

func TestCollectInlineManifestsInDependencyOrder(t *testing.T) {
	configData := []byte("nested inline config")
	configDescriptor := artifact.Descriptor{
		MediaType: "application/vnd.oci.image.config.v1+json",
		Digest:    artifact.CalculateSHA256(configData),
		Size:      int64(len(configData)),
		Data:      inlineData(t, configData),
	}
	leafData, err := json.Marshal(artifact.Manifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		Config:        &configDescriptor,
	})
	if err != nil {
		t.Fatal(err)
	}
	leafDescriptor := inlineManifestDescriptor(t, leafData, "application/vnd.oci.image.manifest.v1+json")
	middleData, err := json.Marshal(artifact.Manifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests:     descriptorList(leafDescriptor),
	})
	if err != nil {
		t.Fatal(err)
	}
	middleDescriptor := inlineManifestDescriptor(t, middleData, "application/vnd.oci.image.index.v1+json")
	rootData, err := json.Marshal(artifact.Manifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests:     descriptorList(middleDescriptor),
	})
	if err != nil {
		t.Fatal(err)
	}
	root, err := artifact.ParseManifest(rootData, "application/vnd.oci.image.index.v1+json", 10, 1<<20)
	if err != nil {
		t.Fatalf("parse root manifest: %v", err)
	}
	limits := config.Limits{ClosureDepth: 3, MaxDescriptors: 10, DescriptorDataBytes: 1 << 20, ManifestBytes: 1 << 20}
	objects, err := collectInlineObjects(artifact.CalculateSHA256(rootData), root, limits)
	if err != nil {
		t.Fatalf("collect inline objects: %v", err)
	}
	if len(objects.blobs) != 1 || string(objects.blobs[configDescriptor.Digest].Embedded) != string(configData) {
		t.Fatalf("inline blobs = %#v", objects.blobs)
	}
	if len(objects.manifests) != 2 || objects.manifests[0].digest != leafDescriptor.Digest || objects.manifests[1].digest != middleDescriptor.Digest {
		t.Fatalf("manifest dependency order = %#v", objects.manifests)
	}
	if string(objects.manifests[0].data) != string(leafData) || string(objects.manifests[1].data) != string(middleData) {
		t.Fatal("inline manifest bytes changed during collection")
	}

	limits.ClosureDepth = 1
	if _, err := collectInlineObjects(artifact.CalculateSHA256(rootData), root, limits); err == nil {
		t.Fatal("expected recursion depth limit error")
	}
	limits.ClosureDepth = 3
	limits.MaxDescriptors = 2
	if _, err := collectInlineObjects(artifact.CalculateSHA256(rootData), root, limits); err == nil {
		t.Fatal("expected aggregate descriptor limit error")
	}
	limits.MaxDescriptors = 10
	limits.DescriptorDataBytes = 1
	if _, err := collectInlineObjects(artifact.CalculateSHA256(rootData), root, limits); err == nil {
		t.Fatal("expected aggregate descriptor.data limit error")
	}
}

func TestEnsureEmbeddedManifestUsesStandardManifestAPI(t *testing.T) {
	data := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	digest := artifact.CalculateSHA256(data)
	stored := false
	var methods []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		methods = append(methods, request.Method)
		if request.URL.Path != "/v2/test/repository/manifests/"+digest {
			t.Errorf("unexpected Distribution path: %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		switch request.Method {
		case http.MethodGet:
			if !stored {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusNotFound)
				_, _ = writer.Write([]byte(`{"errors":[{"code":"MANIFEST_UNKNOWN","message":"not found"}]}`))
				return
			}
			writer.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			writer.Header().Set("Docker-Content-Digest", digest)
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write(data)
		case http.MethodPut:
			if request.Header.Get("Content-Type") != "application/vnd.oci.image.index.v1+json" {
				t.Errorf("PUT Content-Type = %q", request.Header.Get("Content-Type"))
			}
			body, err := io.ReadAll(request.Body)
			if err != nil || string(body) != string(data) {
				t.Errorf("PUT body = %q, err = %v", body, err)
			}
			stored = true
			writer.Header().Set("Docker-Content-Digest", digest)
			writer.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected Distribution method: %s", request.Method)
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	registry := newTestDistribution(t, server)
	defer registry.Close()
	service := &Service{registry: registry, cfg: config.Config{Limits: config.Limits{ManifestBytes: 1 << 20}}}
	manifest := inlineManifest{
		digest: digest, mediaType: "application/vnd.oci.image.index.v1+json", data: data,
		parsed: artifact.Manifest{SchemaVersion: 2, MediaType: "application/vnd.oci.image.index.v1+json"},
	}
	beforeWriteCalls := 0
	err := service.ensureEmbeddedManifest(context.Background(), route.Route{Internal: "test/repository"}, manifest, func() error {
		beforeWriteCalls++
		return nil
	})
	if err != nil {
		t.Fatalf("ensure inline manifest: %v", err)
	}
	if !stored || beforeWriteCalls != 1 || len(methods) != 3 || methods[0] != http.MethodGet || methods[1] != http.MethodPut || methods[2] != http.MethodGet {
		t.Fatalf("stored=%t beforeWrite=%d methods=%v", stored, beforeWriteCalls, methods)
	}
}

func inlineData(t *testing.T, data []byte) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(base64.StdEncoding.EncodeToString(data))
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func inlineManifestDescriptor(t *testing.T, data []byte, mediaType string) artifact.Descriptor {
	t.Helper()
	return artifact.Descriptor{
		MediaType: mediaType,
		Digest:    artifact.CalculateSHA256(data),
		Size:      int64(len(data)),
		Data:      inlineData(t, data),
	}
}
