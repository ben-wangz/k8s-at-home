package main

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

func (runtime *fixtureRuntime) buildPartialManifest() error {
	partialConfig := []byte("partial-inline-config")
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"size":      len(partialConfig),
			"digest":    "sha256:" + hex.EncodeToString(sha256Bytes(partialConfig)),
			"data":      base64.StdEncoding.EncodeToString(partialConfig),
		},
		"layers": []any{map[string]any{
			"mediaType": "application/vnd.oci.image.layer.v1.tar",
			"size":      len(runtime.partialBlob),
			"digest":    runtime.partialBlobDigest,
		}},
	})
	if err != nil {
		return fmt.Errorf("fixture partial response manifest is invalid: %w", err)
	}
	runtime.partialManifestBytes = manifest
	runtime.partialManifestDigest = "sha256:" + hex.EncodeToString(sha256Bytes(manifest))
	return nil
}

func (runtime *fixtureRuntime) buildInlineFixtures() error {
	inlineConfig := []byte("upstream-inline-config")
	runtime.inlineLayer = []byte("upstream-url-layer")
	inlineSubject := []byte("upstream-inline-subject")
	var err error
	runtime.inlineManifest, err = buildInlineManifest(inlineConfig, runtime.inlineLayer, inlineSubject, []string{"https://github.com/v2/registry-hub-test/fixture/url-must-not-fetch"})
	if err != nil {
		return fmt.Errorf("fixture inline manifest is invalid: %w", err)
	}
	runtime.missingURLManifest, err = buildInlineManifest(inlineConfig, []byte("upstream-missing-layer"), nil, []string{"https://github.com/v2/registry-hub-test/fixture/url-must-not-fetch-missing"})
	if err != nil {
		return fmt.Errorf("fixture missing-blob manifest is invalid: %w", err)
	}
	runtime.gcCancellationManifest, err = buildInlineManifest(runtime.config, []byte("gc-cancellation-missing-layer"), nil, []string{"https://github.com/v2/registry-hub-test/fixture/url-must-not-fetch-gc-cancellation"})
	if err != nil {
		return fmt.Errorf("fixture GC-cancellation manifest is invalid: %w", err)
	}
	invalidBody, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"size":      1,
			"digest":    "sha256:" + hex.EncodeToString(sha256Bytes([]byte("x"))),
			"data":      "%%%invalid-base64%%%",
		},
		"layers": []any{},
	})
	if err != nil {
		return fmt.Errorf("fixture invalid inline-data manifest is invalid: %w", err)
	}
	runtime.invalidInlineManifest = fixtureManifest{
		Body: invalidBody, MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest: "sha256:" + hex.EncodeToString(sha256Bytes(invalidBody)),
	}
	runtime.invalidInlineSize, err = buildInlineDataFailureFixture(base64.StdEncoding.EncodeToString([]byte("x")), 2, "sha256:"+hex.EncodeToString(sha256Bytes([]byte("x"))))
	if err != nil {
		return fmt.Errorf("fixture inline-data size-mismatch manifest is invalid: %w", err)
	}
	runtime.invalidInlineDigest, err = buildInlineDataFailureFixture(base64.StdEncoding.EncodeToString([]byte("y")), 1, "sha256:"+hex.EncodeToString(sha256Bytes([]byte("x"))))
	if err != nil {
		return fmt.Errorf("fixture inline-data digest-mismatch manifest is invalid: %w", err)
	}
	runtime.invalidInlineType, err = buildInlineDataFailureFixture(1, 1, "sha256:"+hex.EncodeToString(sha256Bytes([]byte("x"))))
	if err != nil {
		return fmt.Errorf("fixture inline-data type-mismatch manifest is invalid: %w", err)
	}
	return nil
}

func (runtime *fixtureRuntime) buildDiscoveryFixtures() error {
	runtime.nestedConfig = []byte("nested-platform-config")
	runtime.nestedConfigDigest = "sha256:" + hex.EncodeToString(sha256Bytes(runtime.nestedConfig))
	var err error
	runtime.nestedRoot, runtime.nestedObjects, err = buildNestedDiscoveryFixture(runtime.nestedConfig)
	if err != nil {
		return fmt.Errorf("fixture nested OCI index is invalid: %w", err)
	}
	var depthObjects map[string]fixtureManifest
	runtime.depthRoot, depthObjects, err = buildClosureDepthFixture()
	if err != nil {
		return fmt.Errorf("fixture closure-depth OCI index is invalid: %w", err)
	}
	for digest, object := range depthObjects {
		runtime.nestedObjects[digest] = object
	}
	runtime.sharedSizeConflict, err = buildSharedDigestConflictIndex(runtime.manifestBytes, runtime.manifestDigest, "size")
	if err != nil {
		return fmt.Errorf("fixture shared-size conflict is invalid: %w", err)
	}
	runtime.sharedMediaConflict, err = buildSharedDigestConflictIndex(runtime.manifestBytes, runtime.manifestDigest, "mediaType")
	if err != nil {
		return fmt.Errorf("fixture shared-media conflict is invalid: %w", err)
	}
	runtime.dockerListBytes = []byte(dockerListBody)
	runtime.dockerListDigest = "sha256:" + hex.EncodeToString(sha256Bytes(runtime.dockerListBytes))
	return nil
}

func (runtime *fixtureRuntime) registryHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		state := runtime.requestState(request)
		log.Printf("method=%s host=%s path=%s auth=%s scope=%s service=%s range=%q accept=%q", request.Method, state.host, request.URL.Path, state.auth, state.scope, state.service, request.Header.Get("Range"), request.Header.Get("Accept"))
		if runtime.serveBlob(writer, request, state) || runtime.serveTagList(writer, request, state) || runtime.serveDockerRoutes(writer, request, state) || runtime.serveGitHubRoutes(writer, request, state) || runtime.serveOtherRoutes(writer, request, state) || runtime.serveFallbackRoutes(writer, request, state) {
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	})
}

func (runtime *fixtureRuntime) requestState(request *http.Request) fixtureRequestState {
	host := strings.ToLower(request.Host)
	if request.URL.IsAbs() && request.URL.Host != "" {
		host = strings.ToLower(request.URL.Host)
	}
	auth := requestAuthState(request, host, runtime.dockerCredential, runtime.ghcrCredential)
	if request.Header.Get("Proxy-Authorization") != "" {
		auth = "proxy-auth-present"
	}
	return fixtureRequestState{host: host, auth: auth, scope: request.URL.Query().Get("scope"), service: request.URL.Query().Get("service")}
}

func (runtime *fixtureRuntime) serveBlob(writer http.ResponseWriter, request *http.Request, state fixtureRequestState) bool {
	path := request.URL.Path
	if state.host == "github.com" && state.auth == "absent" {
		if blob, ok := runtime.resumeByDigest[strings.TrimPrefix(path, "/v2/registry-hub-test/fixture/blobs/")]; ok && strings.HasPrefix(path, "/v2/registry-hub-test/fixture/blobs/") {
			runtime.serveResumeBlob(writer, request, blob)
			return true
		}
	}
	switch {
	case state.host == "registry-1.docker.io" && path == "/v2/library/registry-hub-test/blobs/"+runtime.configDigest && (state.auth == "basic-matched" || state.auth == "bearer-matched"):
		writeBlob(writer, runtime.config, runtime.configDigest)
	case state.host == "github.com" && path == "/v2/registry-hub-test/fixture/blobs/"+runtime.configDigest && state.auth == "absent":
		writeBlob(writer, runtime.config, runtime.configDigest)
	case state.host == "github.com" && path == "/v2/registry-hub-test/fixture/blobs/"+runtime.discoveryConfigDigest && state.auth == "absent":
		writeBlob(writer, runtime.discoveryConfig, runtime.discoveryConfigDigest)
	case state.host == "github.com" && path == "/v2/registry-hub-test/fixture/blobs/"+runtime.multipartBlobDigest && state.auth == "absent":
		writeBlob(writer, runtime.multipartBlob, runtime.multipartBlobDigest)
	case state.host == "github.com" && path == "/v2/registry-hub-test/fixture/blobs/"+runtime.objectSizeBlobDigest && state.auth == "absent":
		writeBlob(writer, runtime.objectSizeBlob, runtime.objectSizeBlobDigest)
	case state.host == "github.com" && path == "/v2/registry-hub-test/fixture/blobs/"+runtime.backgroundRetryBlobDigest && state.auth == "absent":
		if atomic.AddInt32(&runtime.backgroundRetryRequests, 1) <= 3 {
			writer.Header().Set("Retry-After", "0")
			writer.Header().Set("Connection", "close")
			writer.WriteHeader(http.StatusServiceUnavailable)
		} else {
			writeBlob(writer, runtime.backgroundRetryBlob, runtime.backgroundRetryBlobDigest)
		}
	case state.host == "github.com" && path == "/v2/registry-hub-test/fixture/blobs/"+runtime.rangeBlobDigest && state.auth == "absent":
		if request.Header.Get("Range") != "" {
			writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		} else {
			writeBlob(writer, runtime.rangeBlob, runtime.rangeBlobDigest)
		}
	case state.host == "github.com" && path == "/v2/registry-hub-test/fixture/blobs/"+runtime.partialBlobDigest && state.auth == "absent":
		runtime.servePartialBlob(writer, request)
	case state.host == "github.com" && path == "/v2/registry-hub-test/fixture/blobs/"+runtime.waiterBlobDigest && state.auth == "absent":
		writeDelayedBlob(writer, request, runtime.waiterBlob, runtime.waiterBlobDigest, 36*time.Second)
	case state.host == "github.com" && path == "/v2/registry-hub-test/fixture/blobs/"+runtime.inlineManifest.LayerDigest && state.auth == "absent":
		writeBlob(writer, runtime.inlineLayer, runtime.inlineManifest.LayerDigest)
	case state.host == "github.com" && path == "/v2/registry-hub-test/fixture/blobs/"+runtime.nestedConfigDigest && state.auth == "absent":
		writeBlob(writer, runtime.nestedConfig, runtime.nestedConfigDigest)
	case state.host == "ghcr.io" && path == "/v2/registry-hub-test/fixture/blobs/"+runtime.configDigest && state.auth == "bearer-matched":
		writeBlob(writer, runtime.config, runtime.configDigest)
	case state.host == "tls.registry-hub.test" && path == "/v2/registry-hub-test/fixture/blobs/"+runtime.configDigest && state.auth == "absent":
		writeBlob(writer, runtime.config, runtime.configDigest)
	default:
		return false
	}
	return true
}

func (runtime *fixtureRuntime) servePartialBlob(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Range") != "" {
		writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Content-Length", "3")
	writer.Header().Set("Content-Range", fmt.Sprintf("bytes 0-2/%d", len(runtime.partialBlob)))
	writer.Header().Set("Docker-Content-Digest", runtime.partialBlobDigest)
	writer.WriteHeader(http.StatusPartialContent)
	_, _ = writer.Write(runtime.partialBlob[:3])
}

func (runtime *fixtureRuntime) serveTagList(writer http.ResponseWriter, request *http.Request, state fixtureRequestState) bool {
	if (state.host != "github.com" && state.host != "quay.io") || request.Method != http.MethodGet || request.URL.Path != "/v2/registry-hub-test/tags-only/tags/list" || state.auth != "absent" {
		return false
	}
	writer.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprint(writer, `{"name":"registry-hub-test/tags-only","tags":["alpha","beta","gamma"]}`)
	return true
}

func (runtime *fixtureRuntime) serveDockerRoutes(writer http.ResponseWriter, request *http.Request, state fixtureRequestState) bool {
	path := request.URL.Path
	switch {
	case state.host == "registry-1.docker.io" && path == "/v2/library/registry-hub-test/manifests/authorized" && state.auth == "basic-matched":
		writeManifest(writer, runtime.manifestBytes, runtime.manifestDigest)
	case state.host == "registry-1.docker.io" && path == "/v2/library/registry-hub-test/manifests/token-deadline" && state.auth == "basic-matched":
		writeDockerChallenge(writer, "token-deadline")
	case state.host == "registry-1.docker.io" && path == "/v2/library/registry-hub-test/manifests/token-retry" && state.auth == "basic-matched":
		writeDockerChallenge(writer, "token-retry")
	case state.host == "registry-1.docker.io" && path == "/v2/library/registry-hub-test/manifests/token-retry" && state.auth == "bearer-matched":
		writeManifest(writer, runtime.manifestBytes, runtime.manifestDigest)
	case state.host == "registry-1.docker.io" && path == "/v2/library/registry-hub-test/manifests/challenge" && state.auth == "basic-matched":
		writeDockerChallenge(writer, "registry-hub-fixture")
	case state.host == "registry-1.docker.io" && path == "/v2/library/registry-hub-test/manifests/challenge" && state.auth == "bearer-matched":
		writeManifest(writer, runtime.manifestBytes, runtime.manifestDigest)
	case state.host == "registry-1.docker.io" && path == "/v2/library/registry-hub-test/manifests/redirected" && state.auth == "basic-matched":
		http.Redirect(writer, request, "https://github.com/v2/registry-hub-test/fixture/manifests/redirected", http.StatusFound)
	case state.host == "registry-1.docker.io" && path == "/v2/library/registry-hub-test/manifests/ssrf" && state.auth == "basic-matched":
		writer.Header().Set("Location", "http://127.0.0.1/v2/library/registry-hub-test/manifests/private")
		writer.WriteHeader(http.StatusFound)
	case state.host == "auth.docker.io" && path == "/token" && state.auth == "basic-matched" && state.service == "token-deadline":
		<-request.Context().Done()
	case state.host == "auth.docker.io" && path == "/token" && state.auth == "basic-matched" && state.service == "token-retry":
		if atomic.AddInt32(&runtime.tokenRetryRequests, 1) <= 2 {
			writer.Header().Set("Retry-After", "0")
			writer.Header().Set("Connection", "close")
			writer.WriteHeader(http.StatusServiceUnavailable)
		} else {
			writeToken(writer)
		}
	case state.host == "auth.docker.io" && path == "/token" && state.auth == "basic-matched" && state.scope == "repository:library/registry-hub-test:pull":
		writeToken(writer)
	default:
		return false
	}
	return true
}

func writeDockerChallenge(writer http.ResponseWriter, service string) {
	writer.Header().Set("WWW-Authenticate", `Bearer realm="http://auth.docker.io/token",service="`+service+`"`)
	writer.WriteHeader(http.StatusUnauthorized)
}
