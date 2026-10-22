package main

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

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
