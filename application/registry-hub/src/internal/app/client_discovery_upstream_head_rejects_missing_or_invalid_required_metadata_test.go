package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/artifact"
	"registry-hub/internal/route"
	"testing"
)

func TestUpstreamHeadRejectsMissingOrInvalidRequiredMetadata(t *testing.T) {
	validBlobDigest := artifact.CalculateSHA256([]byte("head response"))
	for _, test := range []struct {
		name   string
		path   string
		result upstreamResult
	}{
		{name: "blob missing digest", path: "/v2/ghcr.io/team/image/blobs/" + validBlobDigest, result: upstreamResult{status: http.StatusOK, body: "head response"}},
		{name: "blob missing length", path: "/v2/ghcr.io/team/image/blobs/" + validBlobDigest, result: upstreamResult{status: http.StatusOK, headers: http.Header{"Docker-Content-Digest": []string{validBlobDigest}}, contentLength: -1, contentLengthSet: true}},
		{name: "blob invalid digest", path: "/v2/ghcr.io/team/image/blobs/" + validBlobDigest, result: upstreamResult{status: http.StatusOK, headers: http.Header{"Docker-Content-Digest": []string{"not-a-digest"}}, body: "head response"}},
		{name: "manifest missing content type", path: "/v2/ghcr.io/team/image/manifests/latest", result: upstreamResult{status: http.StatusOK, headers: http.Header{"Docker-Content-Digest": []string{artifact.CalculateSHA256([]byte(ociManifestBody))}}, body: ociManifestBody}},
		{name: "manifest missing length", path: "/v2/ghcr.io/team/image/manifests/latest", result: upstreamResult{status: http.StatusOK, contentType: ociManifestMediaType, headers: http.Header{"Docker-Content-Digest": []string{artifact.CalculateSHA256([]byte(ociManifestBody))}}, contentLength: -1, contentLengthSet: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			remote := &scriptedUpstream{results: []upstreamResult{test.result}}
			service, manager, _ := newDiscoveryTestService(t, remote)
			routed, err := route.Parse(test.path)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodHead, test.path, nil)
			request.Header.Set("Accept", ociManifestMediaType)
			recorder := httptest.NewRecorder()
			service.serveUpstreamHead(recorder, request, routed, nil, &requestMeta{})
			if recorder.Code != http.StatusBadGateway {
				t.Fatalf("HEAD status = %d, want 502: %s", recorder.Code, recorder.Body.String())
			}
			if len(remote.calls) != 1 || remote.calls[0].method != http.MethodHead {
				t.Fatalf("upstream calls = %#v, want one HEAD and no fallback", remote.calls)
			}
			if got, _, err := manager.List(context.Background(), "", "", 10); err != nil || len(got) != 0 {
				t.Fatalf("HEAD created solidification tasks: %#v, error=%v", got, err)
			}
		})
	}
}
