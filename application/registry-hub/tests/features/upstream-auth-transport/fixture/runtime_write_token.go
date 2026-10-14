package main

import (
	"crypto/tls"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
)

func writeToken(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write([]byte(`{"token":"challenge-fixture-token"}`))
}

func (runtime *fixtureRuntime) serveGitHubRoutes(writer http.ResponseWriter, request *http.Request, state fixtureRequestState) bool {
	if runtime.serveGitHubSpecialManifest(writer, request, state) || runtime.serveGitHubMinimalManifest(writer, request, state) {
		return true
	}
	return false
}

func (runtime *fixtureRuntime) serveGitHubSpecialManifest(writer http.ResponseWriter, request *http.Request, state fixtureRequestState) bool {
	path := request.URL.Path
	switch {
	case state.host == "github.com" && path == githubManifestPrefix+"redirected" && state.auth == "absent":
		writeManifest(writer, runtime.manifestBytes, runtime.manifestDigest)
	case state.host == "github.com" && path == githubManifestPrefix+"isolated" && state.auth == "absent":
		writeManifest(writer, runtime.manifestBytes, runtime.manifestDigest)
	case state.host == "github.com" && path == githubManifestPrefix+"private" && state.auth == "absent":
		writer.Header().Set("WWW-Authenticate", `Basic realm="private-fixture"`)
		writer.WriteHeader(http.StatusUnauthorized)
	case state.host == "github.com" && path == githubManifestPrefix+"minimal-success" && state.auth == "absent":
		writeManifest(writer, runtime.manifestBytes, runtime.manifestDigest)
	default:
		return false
	}
	return true
}

func (runtime *fixtureRuntime) serveGitHubMinimalManifest(writer http.ResponseWriter, request *http.Request, state fixtureRequestState) bool {
	if state.host != "github.com" || state.auth != "absent" {
		return false
	}
	path := strings.TrimPrefix(request.URL.Path, githubManifestPrefix)
	if path == request.URL.Path {
		return false
	}
	switch path {
	case "minimal-multipart":
		writeManifest(writer, runtime.multipartManifestBytes, runtime.multipartManifestDigest)
	case "minimal-object-size-limit":
		writeManifest(writer, runtime.objectSizeManifestBytes, runtime.objectSizeManifestDigest)
	case "minimal-background-retry":
		writeManifestRepresentation(writer, runtime.backgroundRetryManifest, "application/vnd.oci.image.manifest.v1+json", runtime.backgroundRetryManifestID)
	case "minimal-request-deadline":
		atomic.AddInt32(&runtime.requestDeadlineRequests, 1)
		writer.Header().Set("Retry-After", "60")
		writer.WriteHeader(http.StatusServiceUnavailable)
	case "minimal-queue-block":
		<-request.Context().Done()
	case runtime.manifestDigest:
		writeManifest(writer, runtime.manifestBytes, runtime.manifestDigest)
	case runtime.replacementReference:
		writeManifestWithoutDigest(writer, runtime.manifestBytes)
	case "minimal-inline-data":
		writeManifestRepresentation(writer, runtime.inlineManifest.Manifest, "application/vnd.oci.image.manifest.v1+json", runtime.inlineManifest.ManifestDigest)
	case "minimal-invalid-inline-data":
		writeManifestRepresentation(writer, runtime.invalidInlineManifest.Body, runtime.invalidInlineManifest.MediaType, runtime.invalidInlineManifest.Digest)
	case "minimal-invalid-inline-size":
		writeManifestRepresentation(writer, runtime.invalidInlineSize.Body, runtime.invalidInlineSize.MediaType, runtime.invalidInlineSize.Digest)
	case "minimal-invalid-inline-digest":
		writeManifestRepresentation(writer, runtime.invalidInlineDigest.Body, runtime.invalidInlineDigest.MediaType, runtime.invalidInlineDigest.Digest)
	case "minimal-invalid-inline-type":
		writeManifestRepresentation(writer, runtime.invalidInlineType.Body, runtime.invalidInlineType.MediaType, runtime.invalidInlineType.Digest)
	case "minimal-url-missing":
		writeManifestRepresentation(writer, runtime.missingURLManifest.Manifest, "application/vnd.oci.image.manifest.v1+json", runtime.missingURLManifest.ManifestDigest)
	case "minimal-partial-blob":
		writeManifestRepresentation(writer, runtime.partialManifestBytes, "application/vnd.oci.image.manifest.v1+json", runtime.partialManifestDigest)
	case "minimal-shared-size-conflict":
		writeManifestRepresentation(writer, runtime.sharedSizeConflict.Body, ociIndexMediaType, runtime.sharedSizeConflict.Digest)
	case "minimal-shared-media-conflict":
		writeManifestRepresentation(writer, runtime.sharedMediaConflict.Body, ociIndexMediaType, runtime.sharedMediaConflict.Digest)
	case "minimal-discovery":
		runtime.serveDiscoveryManifest(writer, request)
	case "minimal-nested-discovery", "minimal-object-limit", "minimal-byte-limit":
		writeManifestRepresentation(writer, runtime.nestedRoot.Body, runtime.nestedRoot.MediaType, runtime.nestedRoot.Digest)
	case "minimal-depth-limit":
		writeManifestRepresentation(writer, runtime.depthRoot.Body, runtime.depthRoot.MediaType, runtime.depthRoot.Digest)
	case "minimal-compat-unavailable":
		if request.Header.Get("Accept") == dockerListMediaType {
			writer.WriteHeader(http.StatusNotAcceptable)
		} else {
			writeManifestRepresentation(writer, runtime.indexBytes, ociIndexMediaType, runtime.indexDigest)
		}
	case "minimal-compat-503":
		if request.Header.Get("Accept") == dockerListMediaType {
			writer.Header().Set("Retry-After", "0")
			writer.WriteHeader(http.StatusServiceUnavailable)
		} else {
			writeManifestRepresentation(writer, runtime.indexBytes, ociIndexMediaType, runtime.indexDigest)
		}
	case "minimal-compat-invalid":
		writeManifestRepresentation(writer, runtime.indexBytes, ociIndexMediaType, runtime.indexDigest)
	case "minimal-head-405":
		if request.Method == http.MethodHead {
			writer.WriteHeader(http.StatusMethodNotAllowed)
		} else {
			writeManifest(writer, runtime.manifestBytes, runtime.manifestDigest)
		}
	case "minimal-head-success":
		writeManifest(writer, runtime.manifestBytes, runtime.manifestDigest)
	case "minimal-head-request-deadline":
		writer.Header().Set("Retry-After", "60")
		writer.WriteHeader(http.StatusServiceUnavailable)
	case "minimal-missing":
		writer.WriteHeader(http.StatusNotFound)
	case "minimal-unavailable":
		writer.Header().Set("Retry-After", "0")
		writer.WriteHeader(http.StatusServiceUnavailable)
	case "minimal-upstream-401":
		writer.Header().Set("WWW-Authenticate", `Basic realm="https://auth.example/token?secret=upstream-fixture-secret"`)
		writer.WriteHeader(http.StatusUnauthorized)
	case "minimal-upstream-403":
		writer.WriteHeader(http.StatusForbidden)
	case "minimal-upstream-408":
		writer.Header().Set("Retry-After", "0")
		writer.WriteHeader(http.StatusRequestTimeout)
	case "minimal-upstream-429":
		writer.Header().Set("Retry-After", "0")
		writer.WriteHeader(http.StatusTooManyRequests)
	case "minimal-retry-after":
		if atomic.AddInt32(&runtime.retryAfterRequests, 1) <= 2 {
			writer.Header().Set("Retry-After", "5")
			writer.WriteHeader(http.StatusTooManyRequests)
		} else {
			writeManifest(writer, runtime.manifestBytes, runtime.manifestDigest)
		}
	case "minimal-upstream-500":
		writer.Header().Set("Connection", "close")
		writer.WriteHeader(http.StatusInternalServerError)
	case "minimal-upstream-reset":
		serveReset(writer)
	case "minimal-upstream-502":
		writer.Header().Set("Retry-After", "0")
		writer.WriteHeader(http.StatusBadGateway)
	case "minimal-upstream-504":
		writer.Header().Set("Retry-After", "0")
		writer.WriteHeader(http.StatusGatewayTimeout)
	default:
		return false
	}
	return true
}

func (runtime *fixtureRuntime) serveDiscoveryManifest(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Accept") == dockerListMediaType {
		writeManifestRepresentation(writer, runtime.dockerListBytes, dockerListMediaType, runtime.dockerListDigest)
		return
	}
	writeManifestRepresentation(writer, runtime.indexBytes, ociIndexMediaType, runtime.indexDigest)
}

func serveReset(writer http.ResponseWriter) {
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	connection, _, err := hijacker.Hijack()
	if err == nil {
		_ = connection.Close()
	}
}

func (runtime *fixtureRuntime) serveOtherRoutes(writer http.ResponseWriter, request *http.Request, state fixtureRequestState) bool {
	switch {
	case state.host == "ghcr.io" && request.URL.Path == githubManifestPrefix+"bearer" && state.auth == "bearer-matched":
		writeManifest(writer, runtime.manifestBytes, runtime.manifestDigest)
	case state.host == "tls.registry-hub.test" && request.URL.Path == "/v2/registry-hub-test/fixture/manifests/trusted" && state.auth == "absent":
		writeManifest(writer, runtime.manifestBytes, runtime.manifestDigest)
	case state.host == "tls.registry-hub.test" && request.URL.Path == "/v2/registry-hub-test/fixture/manifests/downgrade" && state.auth == "absent":
		http.Redirect(writer, request, "http://github.com/v2/registry-hub-test/fixture/manifests/downgraded", http.StatusFound)
	default:
		return false
	}
	return true
}

func (runtime *fixtureRuntime) serveFallbackRoutes(writer http.ResponseWriter, request *http.Request, state fixtureRequestState) bool {
	path := request.URL.Path
	switch {
	case state.host == "registry-1.docker.io" && path == "/v2/library/registry-hub-test/manifests/authorized":
		writer.Header().Set("WWW-Authenticate", `Basic realm="registry-hub-upstream-fixture"`)
		writer.WriteHeader(http.StatusUnauthorized)
	case state.host == "registry-1.docker.io" && path == "/v2/library/registry-hub-test/manifests/challenge":
		writer.WriteHeader(http.StatusUnauthorized)
	case state.host == "registry-1.docker.io" && (path == "/v2/library/registry-hub-test/manifests/redirected" || path == "/v2/library/registry-hub-test/manifests/ssrf"):
		writer.WriteHeader(http.StatusForbidden)
	case state.host == "github.com" && path == githubManifestPrefix+"redirected":
		writer.WriteHeader(http.StatusForbidden)
	case state.host == "auth.docker.io" && path == "/token":
		writer.WriteHeader(http.StatusForbidden)
	case state.host == "github.com" && path == githubManifestPrefix+"isolated":
		writer.WriteHeader(http.StatusForbidden)
	case state.host == "ghcr.io" && path == githubManifestPrefix+"bearer":
		writer.WriteHeader(http.StatusForbidden)
	case state.host == "github.com" && state.auth == "absent" && nestedManifestPath(path, runtime.nestedObjects):
		digest := strings.TrimPrefix(path, githubManifestPrefix)
		object := runtime.nestedObjects[digest]
		writeManifestRepresentation(writer, object.Body, object.MediaType, object.Digest)
	default:
		return false
	}
	return true
}

func tunnelHandler(handler http.Handler, tlsConfig *tls.Config) http.Handler {
	var fixtureHandler http.Handler
	fixtureHandler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodConnect {
			log.Printf("connect host=%s", strings.ToLower(request.Host))
			serveConnect(writer, request, fixtureHandler, tlsConfig)
			return
		}
		handler.ServeHTTP(writer, request)
	})
	return fixtureHandler
}
