package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/upstream"
	"strings"
	"testing"
)

func TestUnsafeRedirectFailureDoesNotExposeTargetOrAdvanceRoot(t *testing.T) {
	secretTarget := "https://registry-private.example/object?signature=upstream-secret"
	remote := &scriptedUpstream{results: []upstreamResult{{err: fmt.Errorf("%w: %s", upstream.ErrSSRFRejected, secretTarget)}}}
	service, manager, routed := newDiscoveryTestService(t, remote)
	request := httptest.NewRequest(http.MethodGet, "/v2/ghcr.io/team/image/manifests/latest", nil)
	request.Header.Set("Accept", "*/*")
	recorder := httptest.NewRecorder()
	meta := &requestMeta{}
	service.serveUpstreamManifest(recorder, request, routed, meta)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), secretTarget) || strings.Contains(recorder.Header().Get("Location"), secretTarget) {
		t.Fatalf("unsafe redirect target leaked in response: headers=%v body=%s", recorder.Header(), recorder.Body.String())
	}
	if meta.errorClass != "ssrf" {
		t.Fatalf("unsafe redirect error class = %q, want ssrf", meta.errorClass)
	}
	if _, err := service.cache.GetAny(context.Background(), routed.Upstream, routed.RemoteRepo, "latest"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("unsafe redirect created a complete cache object: %v", err)
	}
	task := discoveryTask(t, manager)
	if task.RootDigest != "" || task.Status != tasks.Failed || task.Phase != "discovery" {
		t.Fatalf("unsafe redirect advanced a solidification root: %#v", task)
	}
}

func TestUpstreamNotModifiedPreservesValidatorsWithoutCachingError(t *testing.T) {
	remote := &scriptedUpstream{results: []upstreamResult{
		{status: http.StatusNotModified, headers: http.Header{
			"Etag":          {`"remote-etag"`},
			"Last-Modified": {"Wed, 21 Oct 2015 07:28:00 GMT"},
		}},
		{status: http.StatusOK, contentType: ociManifestMediaType, body: ociManifestBody},
	}}
	service, manager, routed := newDiscoveryTestService(t, remote)
	request := httptest.NewRequest(http.MethodGet, routed.RawPath, nil)
	request.Header.Set("Accept", ociManifestMediaType)
	request.Header.Set("If-None-Match", `"client-etag"`)
	request.Header.Set("If-Modified-Since", "Wed, 21 Oct 2015 07:28:00 GMT")
	recorder := httptest.NewRecorder()
	meta := &requestMeta{}
	service.serveUpstreamManifest(recorder, request, routed, meta)
	if recorder.Code != http.StatusNotModified || recorder.Body.Len() != 0 {
		t.Fatalf("conditional response status=%d body=%q, want bodyless 304", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("ETag") != `"remote-etag"` || recorder.Header().Get("Last-Modified") == "" {
		t.Fatalf("conditional response validators were lost: %v", recorder.Header())
	}
	if meta.cacheSource != "upstream" || remote.calls[0].ifNoneMatch != `"client-etag"` || remote.calls[0].ifModifiedSince == "" {
		t.Fatalf("request metadata or upstream validators are incorrect: meta=%#v call=%#v", meta, remote.calls[0])
	}
	if _, err := service.cache.GetAny(context.Background(), routed.Upstream, routed.RemoteRepo, artifact.CalculateSHA256([]byte(ociManifestBody))); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("304 response created a cache object: %v", err)
	}
	tasksAfterNotModified, _, err := manager.List(context.Background(), "", "", 10)
	if err != nil || len(tasksAfterNotModified) != 1 || tasksAfterNotModified[0].Status != tasks.Failed {
		t.Fatalf("304 discovery state = %#v, error=%v; want one terminal discovery without a root", tasksAfterNotModified, err)
	}

	request = httptest.NewRequest(http.MethodGet, routed.RawPath, nil)
	request.Header.Set("Accept", ociManifestMediaType)
	recorder = httptest.NewRecorder()
	service.serveUpstreamManifest(recorder, request, routed, &requestMeta{})
	if recorder.Code != http.StatusOK || recorder.Body.String() != ociManifestBody {
		t.Fatalf("unconditional retry status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if len(remote.calls) != 2 || remote.calls[1].ifNoneMatch != "" || remote.calls[1].ifModifiedSince != "" {
		t.Fatalf("unconditional retry calls = %#v", remote.calls)
	}
	tasksAfterRetry, _, err := manager.List(context.Background(), "", "", 10)
	if err != nil || len(tasksAfterRetry) != 2 {
		t.Fatalf("unconditional retry task state = %#v, error=%v", tasksAfterRetry, err)
	}
	rootFound := false
	for _, task := range tasksAfterRetry {
		rootFound = rootFound || task.RootDigest == artifact.CalculateSHA256([]byte(ociManifestBody))
	}
	if !rootFound {
		t.Fatalf("unconditional retry did not record the manifest root: %#v", tasksAfterRetry)
	}
}

func TestUpstreamFailureClassifiesDigestAndPolicyErrors(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{err: artifact.ErrDigestMismatch, want: "digest_mismatch"},
		{err: upstream.ErrSSRFRejected, want: "ssrf"},
		{err: upstream.ErrSecurityPolicyRejected, want: "upstream_policy"},
		{err: errors.New("connection reset"), want: "upstream"},
	} {
		if got := upstreamFailureClass(test.err); got != test.want {
			t.Errorf("upstreamFailureClass(%v) = %q, want %q", test.err, got, test.want)
		}
	}
}

func TestAcceptedDiscoveryIndexIsDeliveredWithoutCompatibilityRequest(t *testing.T) {
	remote := &scriptedUpstream{results: []upstreamResult{{status: http.StatusOK, contentType: ociIndexMediaType, body: ociIndexBody}}}
	service, manager, routed := newDiscoveryTestService(t, remote)
	recorder := serveDiscoveryRequest(t, service, routed, ociIndexMediaType)
	if recorder.Code != http.StatusOK || recorder.Body.String() != ociIndexBody {
		t.Fatalf("response status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if len(remote.calls) != 1 {
		t.Fatalf("upstream calls = %d, want one discovery request", len(remote.calls))
	}
	digest := artifact.CalculateSHA256([]byte(ociIndexBody))
	task := discoveryTask(t, manager)
	if task.RootDigest != digest || task.MediaType != ociIndexMediaType || task.Phase != "solidification" {
		t.Fatalf("solidification root = %#v", task)
	}
	if got := cachedBody(t, service, routed.Upstream, routed.RemoteRepo, digest, ociIndexMediaType); string(got) != ociIndexBody {
		t.Fatalf("cached discovery body = %q", got)
	}
}

func TestCompatibilityResponseIsRequestedOnceAndKeptSeparateFromDiscoveryRoot(t *testing.T) {
	remote := &scriptedUpstream{results: []upstreamResult{
		{status: http.StatusOK, contentType: ociIndexMediaType, body: ociIndexBody},
		{status: http.StatusOK, contentType: dockerManifestMediaType, body: dockerManifestBody},
	}}
	service, manager, routed := newDiscoveryTestService(t, remote)
	recorder := serveDiscoveryRequest(t, service, routed, dockerManifestMediaType)
	if recorder.Code != http.StatusOK || recorder.Body.String() != dockerManifestBody {
		t.Fatalf("response status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if len(remote.calls) != 2 || remote.calls[0].method != http.MethodGet || remote.calls[1].method != http.MethodGet {
		t.Fatalf("upstream calls = %#v, want one discovery and one compatibility GET", remote.calls)
	}
	if !strings.Contains(remote.calls[0].accept, ociIndexMediaType) || remote.calls[1].accept != dockerManifestMediaType {
		t.Fatalf("upstream Accept values = %q, %q", remote.calls[0].accept, remote.calls[1].accept)
	}
	indexDigest := artifact.CalculateSHA256([]byte(ociIndexBody))
	manifestDigest := artifact.CalculateSHA256([]byte(dockerManifestBody))
	task := discoveryTask(t, manager)
	if task.RootDigest != indexDigest || task.MediaType != ociIndexMediaType {
		t.Fatalf("compatibility response replaced discovery root: %#v", task)
	}
	if got := cachedBody(t, service, routed.Upstream, routed.RemoteRepo, manifestDigest, dockerManifestMediaType); string(got) != dockerManifestBody {
		t.Fatalf("compatibility cache body = %q", got)
	}
}

func TestCompatibilityFailureKeepsVerifiedDiscoveryRootForSolidification(t *testing.T) {
	remote := &scriptedUpstream{results: []upstreamResult{
		{status: http.StatusOK, contentType: ociIndexMediaType, body: ociIndexBody},
		{status: http.StatusServiceUnavailable, retryAfter: "0"},
		{status: http.StatusServiceUnavailable, retryAfter: "0"},
		{status: http.StatusServiceUnavailable, retryAfter: "0"},
	}}
	service, manager, routed := newDiscoveryTestService(t, remote)
	recorder := serveDiscoveryRequest(t, service, routed, dockerManifestMediaType)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
	if len(remote.calls) != 1+maxUpstreamRetryAttempts {
		t.Fatalf("upstream calls = %d, want discovery plus %d compatibility attempts", len(remote.calls), maxUpstreamRetryAttempts)
	}
	digest := artifact.CalculateSHA256([]byte(ociIndexBody))
	task := discoveryTask(t, manager)
	if task.RootDigest != digest || task.MediaType != ociIndexMediaType || task.Phase != "solidification" || task.Status != tasks.Pending {
		t.Fatalf("verified discovery root did not remain pending for background work: %#v", task)
	}
	if got := cachedBody(t, service, routed.Upstream, routed.RemoteRepo, digest, ociIndexMediaType); string(got) != ociIndexBody {
		t.Fatalf("discovery root cache body = %q", got)
	}
}

func TestCompatibilityFailureMatrixKeepsVerifiedDiscoveryRoot(t *testing.T) {
	for _, test := range []struct {
		name       string
		responses  []upstreamResult
		wantStatus int
		body       string
	}{
		{name: "unauthorized", responses: []upstreamResult{{status: http.StatusUnauthorized}}, wantStatus: http.StatusUnauthorized, body: dockerManifestBody},
		{name: "forbidden", responses: []upstreamResult{{status: http.StatusForbidden}}, wantStatus: http.StatusForbidden, body: dockerManifestBody},
		{name: "not found", responses: []upstreamResult{{status: http.StatusNotFound}}, wantStatus: http.StatusNotFound, body: dockerManifestBody},
		{name: "terminal server error", responses: []upstreamResult{{status: http.StatusInternalServerError}}, wantStatus: http.StatusInternalServerError, body: dockerManifestBody},
		{name: "retry exhaustion", responses: []upstreamResult{
			{status: http.StatusServiceUnavailable, retryAfter: "0"},
			{status: http.StatusServiceUnavailable, retryAfter: "0"},
			{status: http.StatusServiceUnavailable, retryAfter: "0"},
		}, wantStatus: http.StatusBadGateway, body: dockerManifestBody},
		{name: "invalid compatible manifest", responses: []upstreamResult{{status: http.StatusOK, contentType: dockerManifestMediaType, body: `{"schemaVersion":1}`}}, wantStatus: http.StatusBadGateway, body: `{"schemaVersion":1}`},
		{name: "unacceptable compatible media type", responses: []upstreamResult{{status: http.StatusOK, contentType: ociIndexMediaType, body: ociIndexBody}}, wantStatus: http.StatusNotAcceptable, body: ociIndexBody},
	} {
		t.Run(test.name, func(t *testing.T) {
			results := []upstreamResult{{status: http.StatusOK, contentType: ociIndexMediaType, body: ociIndexBody}}
			results = append(results, test.responses...)
			remote := &scriptedUpstream{results: results}
			service, manager, routed := newDiscoveryTestService(t, remote)
			recorder := serveDiscoveryRequest(t, service, routed, dockerManifestMediaType)
			if recorder.Code != test.wantStatus {
				t.Fatalf("compatibility status = %d, want %d: %s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			wantCalls := 1 + len(test.responses)
			if len(remote.calls) != wantCalls {
				t.Fatalf("upstream calls = %d, want %d", len(remote.calls), wantCalls)
			}
			for _, call := range remote.calls[1:] {
				if call.accept != dockerManifestMediaType {
					t.Errorf("compatibility Accept = %q, want %q", call.accept, dockerManifestMediaType)
				}
			}
			rootDigest := artifact.CalculateSHA256([]byte(ociIndexBody))
			task := discoveryTask(t, manager)
			if task.RootDigest != rootDigest || task.MediaType != ociIndexMediaType || task.Phase != "solidification" || task.Status != tasks.Pending {
				t.Fatalf("compatibility failure changed discovery root state: %#v", task)
			}
			if got := cachedBody(t, service, routed.Upstream, routed.RemoteRepo, rootDigest, ociIndexMediaType); string(got) != ociIndexBody {
				t.Fatalf("discovery root cache body = %q", got)
			}
			compatDigest := artifact.CalculateSHA256([]byte(test.body))
			if compatDigest != rootDigest {
				if _, err := service.cache.GetAny(context.Background(), routed.Upstream, routed.RemoteRepo, compatDigest); !errors.Is(err, storage.ErrNotFound) {
					t.Fatalf("failed compatibility response entered cache: %v", err)
				}
			}
		})
	}
}
