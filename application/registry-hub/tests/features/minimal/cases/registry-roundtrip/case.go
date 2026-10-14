package registryroundtrip

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int    `json:"size"`
}

type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "registry-roundtrip")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	client := testkit.NewHTTPClient(30*time.Second, nil)
	repository := "test.invalid/registry-hub/minimal/" + environment.RunID
	base := environment.ClientURL + "/v2/" + repository
	layer, err := createLayer(environment.RunID)
	if err != nil {
		return err
	}
	layerDigest := contentDigest(layer)
	config, err := json.Marshal(map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"rootfs": map[string]any{
			"type": "layers", "diff_ids": []string{layerDigest},
		},
		"config": map[string]any{},
	})
	if err != nil {
		return err
	}
	configDigest := contentDigest(config)
	if err := pushBlob(ctx, client, base, config, configDigest); err != nil {
		return fmt.Errorf("push OCI config blob: %w", err)
	}
	if err := pushBlob(ctx, client, base, layer, layerDigest); err != nil {
		return fmt.Errorf("push OCI layer blob: %w", err)
	}
	image := manifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		Config:        descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: configDigest, Size: len(config)},
		Layers:        []descriptor{{MediaType: "application/vnd.oci.image.layer.v1.tar", Digest: layerDigest, Size: len(layer)}},
	}
	manifestBytes, err := json.Marshal(image)
	if err != nil {
		return err
	}
	manifestDigest := contentDigest(manifestBytes)
	if err := putManifest(ctx, client, base+"/manifests/minimal", manifestBytes, manifestDigest); err != nil {
		return err
	}
	if err := verifyBlob(ctx, client, base, config, configDigest); err != nil {
		return fmt.Errorf("read OCI config blob: %w", err)
	}
	if err := verifyBlob(ctx, client, base, layer, layerDigest); err != nil {
		return fmt.Errorf("read OCI layer blob: %w", err)
	}
	response, err := testkit.Request(ctx, client, http.MethodGet, base+"/manifests/minimal", nil, map[string]string{
		"Accept": "application/vnd.oci.image.manifest.v1+json",
	}, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return fmt.Errorf("OCI manifest read returned %s", response.Status)
	}
	actual, err := testkit.ReadBody(response, 1<<20)
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, manifestBytes) || contentDigest(actual) != manifestDigest {
		return fmt.Errorf("read OCI manifest did not match the uploaded bytes and digest")
	}
	return support.WriteResult(environment, map[string]any{
		"repository":     repository,
		"manifestDigest": manifestDigest,
		"configDigest":   configDigest,
		"layerDigest":    layerDigest,
	})
}

func createLayer(runID string) ([]byte, error) {
	var content bytes.Buffer
	writer := tar.NewWriter(&content)
	file := []byte("registry-hub-minimal-test:" + runID)
	if err := writer.WriteHeader(&tar.Header{
		Name: "fixture.txt", Mode: 0600, Size: int64(len(file)), Typeflag: tar.TypeReg,
		ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatUSTAR,
	}); err != nil {
		return nil, err
	}
	if _, err := writer.Write(file); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return content.Bytes(), nil
}

func pushBlob(ctx context.Context, client *http.Client, base string, content []byte, digest string) error {
	uploadEndpoint := base + "/blobs/uploads/"
	response, err := testkit.Request(ctx, client, http.MethodPost, uploadEndpoint, nil, nil, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusAccepted {
		response.Body.Close()
		return fmt.Errorf("upload initialization returned %s", response.Status)
	}
	location := response.Header.Get("Location")
	response.Body.Close()
	uploadURL, err := testkit.ResolveLocation(uploadEndpoint, location)
	if err != nil {
		return err
	}
	response, err = testkit.Request(ctx, client, http.MethodPatch, uploadURL, bytes.NewReader(content), map[string]string{
		"Content-Type": "application/octet-stream",
	}, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusAccepted {
		response.Body.Close()
		return fmt.Errorf("upload chunk returned %s", response.Status)
	}
	updatedLocation := response.Header.Get("Location")
	response.Body.Close()
	if updatedLocation != "" {
		uploadURL, err = testkit.ResolveLocation(uploadURL, updatedLocation)
		if err != nil {
			return err
		}
	}
	finalURL, err := withDigest(uploadURL, digest)
	if err != nil {
		return err
	}
	response, err = testkit.Request(ctx, client, http.MethodPut, finalURL, strings.NewReader(""), map[string]string{
		"Content-Type": "application/octet-stream",
	}, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusCreated {
		response.Body.Close()
		return fmt.Errorf("blob commit returned %s", response.Status)
	}
	if response.Header.Get("Docker-Content-Digest") != digest {
		response.Body.Close()
		return fmt.Errorf("blob commit returned an unexpected digest")
	}
	response.Body.Close()
	return nil
}

func putManifest(ctx context.Context, client *http.Client, endpoint string, content []byte, digest string) error {
	response, err := testkit.Request(ctx, client, http.MethodPut, endpoint, bytes.NewReader(content), map[string]string{
		"Content-Type": "application/vnd.oci.image.manifest.v1+json",
	}, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusCreated {
		response.Body.Close()
		return fmt.Errorf("manifest upload returned %s", response.Status)
	}
	if response.Header.Get("Docker-Content-Digest") != digest {
		response.Body.Close()
		return fmt.Errorf("manifest upload returned an unexpected digest")
	}
	response.Body.Close()
	return nil
}

func verifyBlob(ctx context.Context, client *http.Client, base string, expected []byte, digest string) error {
	response, err := testkit.Request(ctx, client, http.MethodGet, base+"/blobs/"+digest, nil, nil, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return fmt.Errorf("blob read returned %s", response.Status)
	}
	actual, err := testkit.ReadBody(response, 1<<20)
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, expected) || contentDigest(actual) != digest {
		return fmt.Errorf("read blob did not match the uploaded bytes and digest")
	}
	return nil
}

func withDigest(endpoint, digest string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("digest", digest)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func contentDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}
