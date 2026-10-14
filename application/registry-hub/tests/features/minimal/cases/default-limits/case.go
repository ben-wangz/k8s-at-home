package defaultlimits

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "default-limits")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	manifestDependency := environment.DependencyResults["manifest-validation"]
	repository, err := support.DetailString(manifestDependency, "repository")
	if err != nil {
		return err
	}
	configDigest, err := support.DetailString(manifestDependency, "configDigest")
	if err != nil {
		return err
	}
	layerDigest, err := support.DetailString(manifestDependency, "layerDigest")
	if err != nil {
		return err
	}
	registry := support.NewRegistryClient(environment)
	large := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","size":0,"digest":"` + configDigest + `"},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","size":0,"digest":"` + layerDigest + `"}],"annotations":{"test":"` + strings.Repeat("x", 16<<20) + `"}}`)
	response, err := registry.Request(ctx, http.MethodPut, registry.Endpoint(repository, "/manifests/over-16m"), bytes.NewReader(large), map[string]string{"Content-Type": support.OCIManifestMediaType})
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		response.Body.Close()
		return fmt.Errorf("large manifest returned %s", response.Status)
	}
	response.Body.Close()
	response, err = registry.Get(ctx, repository, "/manifests/over-16m", nil)
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusOK {
		response.Body.Close()
		return fmt.Errorf("oversized manifest was published")
	}
	response.Body.Close()

	descriptorCount := 10001
	descriptors := make([]support.Descriptor, descriptorCount)
	for index := range descriptors {
		descriptors[index] = support.Descriptor{MediaType: support.OCIManifestMediaType, Size: 1, Digest: layerDigest}
	}
	index := support.Manifest{SchemaVersion: 2, MediaType: support.OCIIndexMediaType, Manifests: descriptors}
	indexBytes, err := json.Marshal(index)
	if err != nil {
		return err
	}
	response, err = registry.Request(ctx, http.MethodPut, registry.Endpoint(repository, "/manifests/over-descriptor-limit"), bytes.NewReader(indexBytes), map[string]string{"Content-Type": support.OCIIndexMediaType})
	if err != nil {
		return err
	}
	descriptorLimitStatus := response.StatusCode
	if descriptorLimitStatus != http.StatusBadRequest {
		response.Body.Close()
		return fmt.Errorf("manifest with %d descriptors returned %s", descriptorCount, response.Status)
	}
	response.Body.Close()
	response, err = registry.Get(ctx, repository, "/manifests/over-descriptor-limit", nil)
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusOK {
		response.Body.Close()
		return fmt.Errorf("manifest over the descriptor limit was published")
	}
	response.Body.Close()

	uploadEndpoint := registry.Endpoint(repository, "/blobs/uploads/") + "?digest=" + url.QueryEscape(support.Digest([]byte("declared-too-large")))
	declaredUploadBytes := int64(256<<20) + 1
	uploadStatus, uploadHeaders, err := declaredUploadResponse(ctx, uploadEndpoint, declaredUploadBytes)
	if err != nil {
		return err
	}
	if uploadStatus != http.StatusRequestEntityTooLarge {
		return fmt.Errorf("oversized upload declaration returned HTTP %d", uploadStatus)
	}
	if uploadHeaders.Get("Location") != "" || uploadHeaders.Get("Docker-Upload-UUID") != "" {
		return fmt.Errorf("oversized upload declaration created an upload session")
	}

	tagsRepository, err := support.DetailString(environment.DependencyResults["tags-pagination"], "repository")
	if err != nil {
		return err
	}
	response, err = registry.Tags(ctx, tagsRepository, url.Values{"n": {"1001"}})
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusBadRequest {
		response.Body.Close()
		return fmt.Errorf("tags n=1001 returned %s", response.Status)
	}
	response.Body.Close()
	response, err = registry.Tags(ctx, tagsRepository, url.Values{"n": {"0"}})
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return fmt.Errorf("tags n=0 returned %s", response.Status)
	}
	body, err := testkit.ReadBody(response, 1<<20)
	if err != nil {
		return err
	}
	if !bytes.Contains(body, []byte(`"tags":[]`)) {
		return fmt.Errorf("tags n=0 did not return an empty tag list")
	}
	rateLimitedRequests, retryAfter, err := verifyNoAuthRateLimit(ctx, registry)
	if err != nil {
		return err
	}
	if err := waitForRateLimitRefill(ctx); err != nil {
		return err
	}
	normal := []byte("default-limit-normal:" + environment.RunID)
	normalDigest, err := registry.PutBlob(ctx, repository, normal)
	if err != nil {
		return err
	}
	readNormal, err := support.ReadBlob(ctx, registry, repository, normalDigest)
	if err != nil || !bytes.Equal(readNormal, normal) {
		return fmt.Errorf("normal limit fixture did not round-trip")
	}
	return support.WriteResult(environment, map[string]any{"repository": repository, "manifestStatus": http.StatusRequestEntityTooLarge, "descriptorLimitStatus": descriptorLimitStatus, "descriptorCount": descriptorCount, "uploadStatus": http.StatusRequestEntityTooLarge, "uploadBodySent": false, "uploadSessionCreated": false, "tagsLimitStatus": http.StatusBadRequest, "rateLimitedRequests": rateLimitedRequests, "rateLimitRetryAfter": retryAfter, "largeBytes": len(large), "declaredUploadBytes": strconv.FormatInt(declaredUploadBytes, 10), "normalDigest": normalDigest})
}

func verifyNoAuthRateLimit(ctx context.Context, registry *support.RegistryClient) (int, string, error) {
	const requestCount = 64
	type result struct {
		status     int
		retryAfter string
	}
	results := make(chan result, requestCount)
	failures := make(chan error, requestCount)
	var workers sync.WaitGroup
	for range requestCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			response, err := registry.Version(ctx)
			if err != nil {
				failures <- err
				return
			}
			results <- result{status: response.StatusCode, retryAfter: response.Header.Get("Retry-After")}
			response.Body.Close()
		}()
	}
	workers.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			return 0, "", err
		}
	}
	limited := 0
	retryAfter := ""
	for response := range results {
		switch response.status {
		case http.StatusOK, http.StatusServiceUnavailable:
		case http.StatusTooManyRequests:
			if response.retryAfter == "" {
				return 0, "", fmt.Errorf("rate-limited request has no Retry-After header")
			}
			limited++
			retryAfter = response.retryAfter
		default:
			return 0, "", fmt.Errorf("rate-limit burst returned unexpected HTTP %d", response.status)
		}
	}
	if limited == 0 {
		return 0, "", fmt.Errorf("%d unauthenticated requests did not trigger the source rate limit", requestCount)
	}
	return limited, retryAfter, nil
}

func waitForRateLimitRefill(ctx context.Context) error {
	timer := time.NewTimer(2200 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func declaredUploadResponse(ctx context.Context, endpoint string, declaredLength int64) (int, http.Header, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" {
		return 0, nil, fmt.Errorf("upload endpoint is invalid")
	}
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "tcp", parsed.Host)
	if err != nil {
		return 0, nil, err
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	request := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\nExpect: 100-continue\r\nConnection: close\r\n\r\n", parsed.RequestURI(), parsed.Host, declaredLength)
	if _, err := io.WriteString(connection, request); err != nil {
		return 0, nil, err
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPost})
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	return response.StatusCode, response.Header.Clone(), nil
}
