package podmanpull

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

const corruptRegistryManifestType = "application/vnd.docker.distribution.manifest.v2+json"

type corruptRegistryDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int    `json:"size"`
}

type corruptRegistryFixture struct {
	manifestDigest string
	manifest       []byte
	configDigest   string
	config         []byte
	layerDigest    string
	layer          []byte
	corruptLayer   []byte
}

var zeroTime = time.Unix(0, 0).UTC()

func verifyPodmanRejectsCorruptLayer(ctx context.Context, root string) error {
	fixture, err := newCorruptRegistryFixture()
	if err != nil {
		return err
	}
	server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	defer server.Close()
	podmanRoot, err := os.MkdirTemp("", "rhc-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(podmanRoot)
	storageRoot, err := os.MkdirTemp(podmanRoot, "storage-")
	if err != nil {
		return err
	}
	runRoot := filepath.Join(podmanRoot, "runroot")
	reference := server.Listener.Addr().String() + "/corrupt:latest"
	_, pullErr := testkit.Command(ctx, "podman", "--root", filepath.Join(storageRoot, "store"), "--runroot", runRoot,
		"--storage-driver=vfs", "pull", "--tls-verify=false", "docker://"+reference)
	if pullErr == nil {
		return fmt.Errorf("pull of %s succeeded despite a blob digest mismatch", reference)
	}
	if !strings.Contains(strings.ToLower(pullErr.Error()), "digest") {
		return fmt.Errorf("pull failed without reporting a digest error: %w", pullErr)
	}
	return nil
}

func newCorruptRegistryFixture() (corruptRegistryFixture, error) {
	layer, diffID, err := gzipLayer("verified-layer-content")
	if err != nil {
		return corruptRegistryFixture{}, err
	}
	corruptLayer, _, err := gzipLayer("verified-layer-contenx")
	if err != nil {
		return corruptRegistryFixture{}, err
	}
	if len(corruptLayer) != len(layer) {
		return corruptRegistryFixture{}, fmt.Errorf("corrupt fixture layer size %d differs from declared size %d", len(corruptLayer), len(layer))
	}
	config, err := json.Marshal(map[string]any{
		"architecture": "amd64", "os": "linux",
		"rootfs":  map[string]any{"type": "layers", "diff_ids": []string{diffID}},
		"history": []any{map[string]any{"created_by": "registry-hub-corruption-fixture"}},
	})
	if err != nil {
		return corruptRegistryFixture{}, err
	}
	fixture := corruptRegistryFixture{layer: layer, corruptLayer: corruptLayer, config: config, layerDigest: contentDigest(layer), configDigest: contentDigest(config)}
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": corruptRegistryManifestType,
		"config": corruptRegistryDescriptor{MediaType: "application/vnd.docker.container.image.v1+json", Digest: fixture.configDigest, Size: len(config)},
		"layers": []corruptRegistryDescriptor{{MediaType: "application/vnd.docker.image.rootfs.diff.tar.gzip", Digest: fixture.layerDigest, Size: len(layer)}},
	})
	if err != nil {
		return corruptRegistryFixture{}, err
	}
	fixture.manifest = manifest
	fixture.manifestDigest = contentDigest(manifest)
	return fixture, nil
}

func gzipLayer(content string) ([]byte, string, error) {
	var layer bytes.Buffer
	compressor, err := gzip.NewWriterLevel(&layer, gzip.NoCompression)
	if err != nil {
		return nil, "", err
	}
	compressor.Header.ModTime = zeroTime
	archive := tar.NewWriter(compressor)
	header := &tar.Header{Name: "payload.txt", Mode: 0600, Size: int64(len(content)), Typeflag: tar.TypeReg}
	if err := archive.WriteHeader(header); err != nil {
		return nil, "", err
	}
	if _, err := io.WriteString(archive, content); err != nil {
		return nil, "", err
	}
	if err := archive.Close(); err != nil {
		return nil, "", err
	}
	if err := compressor.Close(); err != nil {
		return nil, "", err
	}
	return layer.Bytes(), contentDigest(tarLayerBytes(content)), nil
}

func tarLayerBytes(content string) []byte {
	var layer bytes.Buffer
	archive := tar.NewWriter(&layer)
	_ = archive.WriteHeader(&tar.Header{Name: "payload.txt", Mode: 0600, Size: int64(len(content)), Typeflag: tar.TypeReg})
	_, _ = io.WriteString(archive, content)
	_ = archive.Close()
	return layer.Bytes()
}

func contentDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (f corruptRegistryFixture) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	if request.URL.Path == "/v2/" {
		writer.WriteHeader(http.StatusOK)
		return
	}
	path := strings.TrimPrefix(request.URL.Path, "/v2/corrupt/")
	switch {
	case path == "manifests/latest":
		writeRegistryFixtureObject(writer, request, corruptRegistryManifestType, f.manifestDigest, f.manifest)
	case path == "blobs/"+f.configDigest:
		writeRegistryFixtureObject(writer, request, "application/octet-stream", f.configDigest, f.config)
	case path == "blobs/"+f.layerDigest:
		writeRegistryFixtureObject(writer, request, "application/octet-stream", f.layerDigest, f.corruptLayer)
	default:
		http.NotFound(writer, request)
	}
}

func writeRegistryFixtureObject(writer http.ResponseWriter, request *http.Request, mediaType, digest string, body []byte) {
	writer.Header().Set("Content-Type", mediaType)
	writer.Header().Set("Content-Length", fmt.Sprint(len(body)))
	writer.Header().Set("Docker-Content-Digest", digest)
	writer.WriteHeader(http.StatusOK)
	if request.Method != http.MethodHead {
		_, _ = writer.Write(body)
	}
}
