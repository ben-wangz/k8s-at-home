package referrersfallback

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func waitProbe(ctx context.Context, environment testkit.CaseContext, name string) (string, error) {
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	for {
		result, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", name, "-c", "probe", "--tail=20")
		if err == nil && strings.Contains(string(result.Stdout), "native-probe:") {
			return string(result.Stdout), nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			return "", fmt.Errorf("native Referrers probe did not report a status")
		case <-time.After(time.Second):
		}
	}
}

func readIndex(ctx context.Context, registry *support.RegistryClient, repository, path string, wantStatus int) ([]map[string]any, error) {
	response, err := registry.Get(ctx, repository, path, map[string]string{"Accept": support.OCIIndexMediaType})
	if err != nil {
		return nil, err
	}
	if response.StatusCode != wantStatus {
		response.Body.Close()
		return nil, fmt.Errorf("index GET %s returned %d, want %d", path, response.StatusCode, wantStatus)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != support.OCIIndexMediaType {
		response.Body.Close()
		return nil, fmt.Errorf("index GET %s has an unexpected Content-Type", path)
	}
	body, err := testkit.ReadBody(response, 4<<20)
	if err != nil {
		return nil, err
	}
	var document struct {
		SchemaVersion int              `json:"schemaVersion"`
		MediaType     string           `json:"mediaType"`
		Manifests     []map[string]any `json:"manifests"`
	}
	if err := json.Unmarshal(body, &document); err != nil || document.SchemaVersion != 2 || document.MediaType != support.OCIIndexMediaType {
		return nil, fmt.Errorf("index GET %s body is invalid", path)
	}
	return document.Manifests, nil
}

func verifyFallbackTag(ctx context.Context, registry *support.RegistryClient, repository, tag, referrerDigest string) (string, error) {
	response, err := registry.Get(ctx, repository, "/manifests/"+tag, map[string]string{"Accept": support.OCIIndexMediaType})
	if err != nil {
		return "", err
	}
	indexDigest := response.Header.Get("Docker-Content-Digest")
	manifestDescriptors, body, err := decodeIndexResponse(response)
	if err != nil {
		return "", err
	}
	if !hasDescriptor(manifestDescriptors, referrerDigest, "application/vnd.example.signature") {
		return "", fmt.Errorf("fallback tag index omitted the published referrer")
	}
	if indexDigest == "" {
		indexDigest = support.Digest(body)
	}
	tagsResponse, err := registry.Tags(ctx, repository, nil)
	if err != nil {
		return "", err
	}
	var tags map[string]any
	if _, err := support.DecodeJSON(tagsResponse, 4<<20, &tags); err != nil {
		return "", err
	}
	for _, raw := range testkit.Slice(tags["tags"]) {
		if testkit.String(raw) == tag {
			return indexDigest, nil
		}
	}
	return "", fmt.Errorf("fallback tag is absent from tags/list")
}

func decodeIndexResponse(response *http.Response) ([]map[string]any, []byte, error) {
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, nil, fmt.Errorf("fallback manifest GET returned %d, want 200", response.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != support.OCIIndexMediaType {
		response.Body.Close()
		return nil, nil, fmt.Errorf("fallback manifest is not an OCI index")
	}
	var document struct {
		SchemaVersion int              `json:"schemaVersion"`
		MediaType     string           `json:"mediaType"`
		Manifests     []map[string]any `json:"manifests"`
	}
	body, err := testkit.ReadBody(response, 4<<20)
	if err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, nil, err
	}
	if document.SchemaVersion != 2 || document.MediaType != support.OCIIndexMediaType {
		return nil, nil, fmt.Errorf("fallback manifest body is invalid")
	}
	return document.Manifests, body, nil
}

func verifyFallbackProtection(ctx context.Context, registry *support.RegistryClient, repository, tag, indexDigest string, referrer []byte, referrerDigest string) (map[string]any, error) {
	headResponse, err := registry.Head(ctx, repository, "/manifests/"+tag, map[string]string{"Accept": support.OCIIndexMediaType})
	if err != nil {
		return nil, err
	}
	headStatus := headResponse.StatusCode
	headBody, readErr := testkit.ReadBody(headResponse, 4<<20)
	if readErr != nil {
		return nil, readErr
	}
	if headStatus != http.StatusOK || len(headBody) != 0 {
		return nil, fmt.Errorf("fallback tag HEAD returned %d with %d body bytes, want 200 and no body", headStatus, len(headBody))
	}

	putResponse, err := registry.Request(ctx, http.MethodPut, registry.Endpoint(repository, "/manifests/")+tag, bytes.NewReader(referrer), map[string]string{"Content-Type": support.OCIManifestMediaType})
	if err != nil {
		return nil, err
	}
	putStatus := putResponse.StatusCode
	putResponse.Body.Close()
	if putStatus != http.StatusConflict {
		return nil, fmt.Errorf("fallback tag overwrite returned %d, want 409", putStatus)
	}
	digestTagURL := registry.Endpoint(repository, "/manifests/") + referrerDigest + "?" + url.Values{"tag": []string{tag}}.Encode()
	digestTagResponse, err := registry.Request(ctx, http.MethodPut, digestTagURL, bytes.NewReader(referrer), map[string]string{"Content-Type": support.OCIManifestMediaType})
	if err != nil {
		return nil, err
	}
	digestTagStatus := digestTagResponse.StatusCode
	digestTagResponse.Body.Close()
	if digestTagStatus != http.StatusConflict {
		return nil, fmt.Errorf("digest PUT with protected tag query returned %d, want 409", digestTagStatus)
	}
	deleteTag, err := registry.Delete(ctx, repository, "/manifests/"+tag)
	if err != nil {
		return nil, err
	}
	deleteTagStatus := deleteTag.StatusCode
	deleteTag.Body.Close()
	if deleteTagStatus != http.StatusConflict {
		return nil, fmt.Errorf("fallback tag delete returned %d, want 409", deleteTagStatus)
	}
	deleteDigest, err := registry.Delete(ctx, repository, "/manifests/"+indexDigest)
	if err != nil {
		return nil, err
	}
	deleteDigestStatus := deleteDigest.StatusCode
	deleteDigest.Body.Close()
	if deleteDigestStatus != http.StatusConflict {
		return nil, fmt.Errorf("fallback index digest delete returned %d, want 409", deleteDigestStatus)
	}
	if response, err := registry.Get(ctx, repository, "/manifests/"+referrerDigest, nil); err != nil {
		return nil, err
	} else {
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fallback referrer manifest returned %d before delete, want 200", response.StatusCode)
		}
	}
	return map[string]any{
		"headStatus": headStatus, "headBodyBytes": len(headBody),
		"tagPutStatus": putStatus, "digestPutTagStatus": digestTagStatus,
		"tagDeleteStatus": deleteTagStatus, "indexDigestDeleteStatus": deleteDigestStatus,
	}, nil
}

type fallbackAccessEvent struct {
	method string
	path   string
	status int
}
