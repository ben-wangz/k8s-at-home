package support

import (
	"bytes"
	"context"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	OCIManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	OCIIndexMediaType    = "application/vnd.oci.image.index.v1+json"
	DockerManifestMedia  = "application/vnd.docker.distribution.manifest.v2+json"
	DockerListMedia      = "application/vnd.docker.distribution.manifest.list.v2+json"
)

type Descriptor struct {
	MediaType    string            `json:"mediaType"`
	Size         int               `json:"size"`
	Digest       string            `json:"digest"`
	URLs         []string          `json:"urls,omitempty"`
	Data         string            `json:"data,omitempty"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Platform     map[string]any    `json:"platform,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        *Descriptor       `json:"config,omitempty"`
	Layers        []Descriptor      `json:"layers,omitempty"`
	Manifests     []Descriptor      `json:"manifests,omitempty"`
	Subject       *Descriptor       `json:"subject,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

type RegistryClient struct {
	Base          string
	Client        *http.Client
	Basic         *testkit.BasicCredential
	Authorization string
}

func NewRegistryClient(environment testkit.CaseContext) *RegistryClient {
	return &RegistryClient{Base: strings.TrimRight(environment.ClientURL, "/"), Client: testkit.NewHTTPClient(30*time.Second, nil)}
}

func (registry *RegistryClient) Endpoint(repository, suffix string) string {
	return registry.Base + "/v2/" + repository + suffix
}

func (registry *RegistryClient) Request(ctx context.Context, method, endpoint string, body io.Reader, headers map[string]string) (*http.Response, error) {
	requestHeaders := make(map[string]string, len(headers)+1)
	for name, value := range headers {
		requestHeaders[name] = value
	}
	if registry.Authorization != "" {
		requestHeaders["Authorization"] = registry.Authorization
	}
	return testkit.Request(ctx, registry.Client, method, endpoint, body, requestHeaders, registry.Basic)
}

func (registry *RegistryClient) Version(ctx context.Context) (*http.Response, error) {
	return registry.Request(ctx, http.MethodGet, registry.Base+"/v2/", nil, nil)
}

func (registry *RegistryClient) Upload(ctx context.Context, repository string, content []byte, digest string) (string, error) {
	endpoint := registry.Endpoint(repository, "/blobs/uploads/")
	response, err := registry.Request(ctx, http.MethodPost, endpoint, nil, nil)
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusAccepted && response.StatusCode != http.StatusCreated {
		defer response.Body.Close()
		return "", fmt.Errorf("upload creation returned %s", response.Status)
	}
	if response.StatusCode == http.StatusCreated {
		response.Body.Close()
		return digest, nil
	}
	location := response.Header.Get("Location")
	response.Body.Close()
	if location == "" {
		return "", fmt.Errorf("upload creation returned no Location")
	}
	uploadURL, err := testkit.ResolveLocation(endpoint, location)
	if err != nil {
		return "", err
	}
	if len(content) > 0 {
		headers := map[string]string{"Content-Type": "application/octet-stream"}
		if len(content) > 1 {
			headers["Content-Range"] = fmt.Sprintf("0-%d", len(content)-1)
		}
		response, err = registry.Request(ctx, http.MethodPatch, uploadURL, bytes.NewReader(content), headers)
		if err != nil {
			return "", err
		}
		if response.StatusCode != http.StatusAccepted {
			defer response.Body.Close()
			return "", fmt.Errorf("upload patch returned %s", response.Status)
		}
		if updated := response.Header.Get("Location"); updated != "" {
			uploadURL, err = testkit.ResolveLocation(uploadURL, updated)
			if err != nil {
				response.Body.Close()
				return "", err
			}
		}
		response.Body.Close()
	}
	finalURL, err := AddQuery(uploadURL, "digest", digest)
	if err != nil {
		return "", err
	}
	response, err = registry.Request(ctx, http.MethodPut, finalURL, strings.NewReader(""), map[string]string{"Content-Type": "application/octet-stream"})
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return "", fmt.Errorf("upload commit returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	if actual := response.Header.Get("Docker-Content-Digest"); actual != "" && actual != digest {
		return "", fmt.Errorf("upload commit digest %q does not match %q", actual, digest)
	}
	return digest, nil
}

func (registry *RegistryClient) CreateUpload(ctx context.Context, repository string, query url.Values) (*http.Response, string, error) {
	endpoint := registry.Endpoint(repository, "/blobs/uploads/")
	if len(query) != 0 {
		endpoint += "?" + query.Encode()
	}
	response, err := registry.Request(ctx, http.MethodPost, endpoint, nil, nil)
	if err != nil {
		return nil, "", err
	}
	if response.StatusCode != http.StatusAccepted {
		return response, "", fmt.Errorf("upload creation returned %s", response.Status)
	}
	location := response.Header.Get("Location")
	if location == "" {
		response.Body.Close()
		return response, "", fmt.Errorf("upload creation returned no Location")
	}
	resolved, err := testkit.ResolveLocation(endpoint, location)
	if err != nil {
		response.Body.Close()
		return response, "", err
	}
	return response, resolved, nil
}

func (registry *RegistryClient) PatchUpload(ctx context.Context, endpoint string, content []byte, start int) (*http.Response, string, error) {
	headers := map[string]string{"Content-Type": "application/octet-stream"}
	if len(content) > 0 {
		headers["Content-Range"] = fmt.Sprintf("%d-%d", start, start+len(content)-1)
	}
	response, err := registry.Request(ctx, http.MethodPatch, endpoint, bytes.NewReader(content), headers)
	if err != nil {
		return nil, "", err
	}
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return response, "", fmt.Errorf("upload patch returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	location := endpoint
	if raw := response.Header.Get("Location"); raw != "" {
		location, err = testkit.ResolveLocation(endpoint, raw)
		if err != nil {
			response.Body.Close()
			return response, "", err
		}
	}
	return response, location, nil
}

func (registry *RegistryClient) CommitUpload(ctx context.Context, endpoint, digest string) (*http.Response, error) {
	commitURL, err := AddQuery(endpoint, "digest", digest)
	if err != nil {
		return nil, err
	}
	return registry.Request(ctx, http.MethodPut, commitURL, strings.NewReader(""), map[string]string{"Content-Type": "application/octet-stream"})
}

func (registry *RegistryClient) CancelUpload(ctx context.Context, endpoint string) (*http.Response, error) {
	return registry.Request(ctx, http.MethodDelete, endpoint, nil, nil)
}

func (registry *RegistryClient) PutBlob(ctx context.Context, repository string, content []byte) (string, error) {
	digest := Digest(content)
	return registry.Upload(ctx, repository, content, digest)
}

func (registry *RegistryClient) PutManifest(ctx context.Context, repository, reference, mediaType string, content []byte, query url.Values) (*http.Response, string, error) {
	endpoint := registry.Endpoint(repository, "/manifests/") + reference
	if len(query) != 0 {
		endpoint += "?" + query.Encode()
	}
	response, err := registry.Request(ctx, http.MethodPut, endpoint, bytes.NewReader(content), map[string]string{"Content-Type": mediaType})
	if err != nil {
		return nil, "", err
	}
	digest := Digest(content)
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return response, digest, fmt.Errorf("manifest PUT returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	if actual := response.Header.Get("Docker-Content-Digest"); actual != "" && actual != digest {
		response.Body.Close()
		return nil, digest, fmt.Errorf("manifest digest %q does not match %q", actual, digest)
	}
	return response, digest, nil
}

func (registry *RegistryClient) Get(ctx context.Context, repository, suffix string, headers map[string]string) (*http.Response, error) {
	return registry.Request(ctx, http.MethodGet, registry.Endpoint(repository, suffix), nil, headers)
}

func (registry *RegistryClient) Head(ctx context.Context, repository, suffix string, headers map[string]string) (*http.Response, error) {
	return registry.Request(ctx, http.MethodHead, registry.Endpoint(repository, suffix), nil, headers)
}

func (registry *RegistryClient) Delete(ctx context.Context, repository, suffix string) (*http.Response, error) {
	return registry.Request(ctx, http.MethodDelete, registry.Endpoint(repository, suffix), nil, nil)
}

func (registry *RegistryClient) Tags(ctx context.Context, repository string, query url.Values) (*http.Response, error) {
	endpoint := registry.Endpoint(repository, "/tags/list")
	if len(query) != 0 {
		endpoint += "?" + query.Encode()
	}
	return registry.Request(ctx, http.MethodGet, endpoint, nil, nil)
}
