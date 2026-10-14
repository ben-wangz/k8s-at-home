package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"registry-hub/internal/config"
	"strings"
	"testing"
)

func TestBearerChallengesKeepAuthorityCredentialsAndRepositoryScopesSeparate(t *testing.T) {
	type observation struct {
		tokenAuth string
		scopes    []string
		registry  []string
	}
	observations := map[string]*observation{
		"8.8.8.8": {},
		"1.1.1.1": {},
	}
	client, err := New(config.Config{UpstreamAuth: map[string]config.UpstreamAuth{
		"8.8.8.8": {Type: "basic", Username: "registry-a", Password: "password-a"},
		"1.1.1.1": {Type: "basic", Username: "registry-b", Password: "password-b"},
	}})
	if err != nil {
		t.Fatalf("create upstream client: %v", err)
	}
	defer client.Close()
	client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: request}
		if request.URL.Path == "/token" {
			observed := observations[request.URL.Host]
			observed.scopes = append(observed.scopes, request.URL.Query().Get("scope"))
			username, _, _ := request.BasicAuth()
			observed.tokenAuth = username
			token := "token-" + request.URL.Host
			response.Body = io.NopCloser(strings.NewReader(`{"token":"` + token + `"}`))
			return response, nil
		}
		observed := observations[request.URL.Host]
		authorization := request.Header.Get("Authorization")
		if !strings.HasPrefix(authorization, "Bearer ") {
			response.StatusCode = http.StatusUnauthorized
			response.Header.Set("WWW-Authenticate", `Bearer realm="https://`+request.URL.Host+`/token",service="`+request.URL.Host+`"`)
			return response, nil
		}
		observed.registry = append(observed.registry, authorization)
		return response, nil
	})

	for _, request := range []struct{ authority, repository string }{
		{authority: "8.8.8.8", repository: "team/alpha"},
		{authority: "1.1.1.1", repository: "team/beta"},
		{authority: "8.8.8.8", repository: "team/gamma"},
	} {
		response, err := client.Do(context.Background(), request.authority, request.repository, http.MethodGet,
			"/v2/"+request.repository+"/manifests/latest", nil, nil)
		if err != nil {
			t.Fatalf("request %s/%s: %v", request.authority, request.repository, err)
		}
		_ = response.Body.Close()
	}

	first, second := observations["8.8.8.8"], observations["1.1.1.1"]
	if first.tokenAuth != "registry-a" || strings.Join(first.scopes, ",") != "repository:team/alpha:pull,repository:team/gamma:pull" {
		t.Fatalf("first authority token credentials/scopes = %q/%v", first.tokenAuth, first.scopes)
	}
	if second.tokenAuth != "registry-b" || strings.Join(second.scopes, ",") != "repository:team/beta:pull" {
		t.Fatalf("second authority token credentials/scopes = %q/%v", second.tokenAuth, second.scopes)
	}
	if strings.Join(first.registry, ",") != "Bearer token-8.8.8.8,Bearer token-8.8.8.8" || strings.Join(second.registry, ",") != "Bearer token-1.1.1.1" {
		t.Fatalf("registry tokens by authority = %v and %v", first.registry, second.registry)
	}
}

func TestBearerChallengeRejectsUnallowlistedTokenAuthority(t *testing.T) {
	var tokenRequests int
	client, err := New(config.Config{UpstreamAuth: map[string]config.UpstreamAuth{
		"8.8.8.8": {Type: "basic", Username: "registry-user", Password: "registry-secret"},
	}})
	if err != nil {
		t.Fatalf("create upstream client: %v", err)
	}
	defer client.Close()
	client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "8.8.8.8" || request.URL.Path == "/token" {
			tokenRequests++
		}
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Www-Authenticate": {`Bearer realm="https://auth.other.example/token",service="registry"`}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}, nil
	})
	response, err := client.Do(context.Background(), "8.8.8.8", "team/image", http.MethodGet,
		"/v2/team/image/manifests/latest", nil, nil)
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "outside the authority allowlist") {
		t.Fatalf("request error = %v, want unallowlisted token authority rejection", err)
	}
	if tokenRequests != 0 {
		t.Fatalf("unallowlisted token endpoint received %d requests", tokenRequests)
	}
}

func TestCrossHostRedirectChainReassignsCredentialsAtEveryHop(t *testing.T) {
	client, err := New(config.Config{UpstreamAuth: map[string]config.UpstreamAuth{
		"8.8.8.8": {Type: "basic", Username: "source", Password: "source-secret"},
		"1.1.1.1": {Type: "basic", Username: "middle", Password: "middle-secret"},
		"9.9.9.9": {Type: "basic", Username: "target", Password: "target-secret"},
	}})
	if err != nil {
		t.Fatalf("create upstream client: %v", err)
	}
	defer client.Close()
	type requestSnapshot struct{ host, authorization, cookie string }
	var requests []requestSnapshot
	client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests = append(requests, requestSnapshot{
			host: request.URL.Host, authorization: request.Header.Get("Authorization"), cookie: request.Header.Get("Cookie"),
		})
		response := &http.Response{StatusCode: http.StatusFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}
		switch request.URL.Host {
		case "8.8.8.8":
			response.Header.Set("Location", "https://1.1.1.1/v2/team/image/manifests/latest")
		case "1.1.1.1":
			response.Header.Set("Location", "https://9.9.9.9/v2/team/image/manifests/latest")
		case "9.9.9.9":
			response.StatusCode = http.StatusOK
		default:
			return nil, errors.New("unexpected redirect authority")
		}
		return response, nil
	})
	response, err := client.Do(context.Background(), "8.8.8.8", "team/image", http.MethodGet,
		"/v2/team/image/manifests/latest", nil, http.Header{"Cookie": {"caller=secret"}, "Authorization": {"Bearer caller-secret"}})
	if err != nil {
		t.Fatalf("redirected request: %v", err)
	}
	_ = response.Body.Close()
	want := []requestSnapshot{
		{host: "8.8.8.8", authorization: "Basic c291cmNlOnNvdXJjZS1zZWNyZXQ="},
		{host: "1.1.1.1", authorization: "Basic bWlkZGxlOm1pZGRsZS1zZWNyZXQ="},
		{host: "9.9.9.9", authorization: "Basic dGFyZ2V0OnRhcmdldC1zZWNyZXQ="},
	}
	if len(requests) != len(want) {
		t.Fatalf("redirect requests = %#v, want %d hops", requests, len(want))
	}
	for index := range want {
		if requests[index] != want[index] {
			t.Fatalf("redirect request %d = %#v, want %#v", index, requests[index], want[index])
		}
	}
}

func TestConditionalValidatorsSurviveRedirectAndPreserveNotModifiedResponse(t *testing.T) {
	client, err := New(config.Config{})
	if err != nil {
		t.Fatalf("create upstream client: %v", err)
	}
	defer client.Close()
	var requests []string
	client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request.URL.Host+" "+request.Header.Get("If-None-Match")+" "+request.Header.Get("If-Modified-Since"))
		response := &http.Response{Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}
		if request.URL.Host == "8.8.8.8" {
			response.StatusCode = http.StatusFound
			response.Header.Set("Location", "https://1.1.1.1/v2/team/image/manifests/latest")
			return response, nil
		}
		if request.URL.Host != "1.1.1.1" {
			return nil, errors.New("unexpected redirect authority")
		}
		response.StatusCode = http.StatusNotModified
		response.Header.Set("ETag", `"remote-etag"`)
		return response, nil
	})

	validators := http.Header{
		"If-None-Match":     {`"client-etag"`},
		"If-Modified-Since": {"Wed, 21 Oct 2015 07:28:00 GMT"},
	}
	response, err := client.Do(context.Background(), "8.8.8.8", "team/image", http.MethodGet,
		"/v2/team/image/manifests/latest", nil, validators)
	if err != nil {
		t.Fatalf("conditional request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotModified || response.Header.Get("ETag") != `"remote-etag"` {
		t.Fatalf("response status=%d ETag=%q, want 304 and remote validator", response.StatusCode, response.Header.Get("ETag"))
	}
	want := []string{
		"8.8.8.8 \"client-etag\" Wed, 21 Oct 2015 07:28:00 GMT",
		"1.1.1.1 \"client-etag\" Wed, 21 Oct 2015 07:28:00 GMT",
	}
	if len(requests) != len(want) {
		t.Fatalf("upstream requests = %v, want %d redirect hops", requests, len(want))
	}
	for index := range want {
		if requests[index] != want[index] {
			t.Errorf("request %d = %q, want %q", index, requests[index], want[index])
		}
	}
}

func TestSafeRequestHeadersUsesAllowlist(t *testing.T) {
	got := safeRequestHeaders(http.Header{
		"Accept":              {"application/vnd.oci.image.manifest.v1+json"},
		"Range":               {"bytes=0-3"},
		"If-None-Match":       {"\"etag\""},
		"If-Modified-Since":   {"Wed, 21 Oct 2015 07:28:00 GMT"},
		"Authorization":       {"Bearer caller-secret"},
		"Cookie":              {"session=secret"},
		"Proxy-Authorization": {"Basic caller-secret"},
		"X-Internal":          {"must-not-forward"},
	})
	for _, name := range []string{"Accept", "Range", "If-None-Match", "If-Modified-Since"} {
		if got.Get(name) == "" {
			t.Errorf("allowed header %s was dropped", name)
		}
	}
	for _, name := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-Internal"} {
		if got.Get(name) != "" {
			t.Errorf("unsafe header %s was retained: %q", name, got.Get(name))
		}
	}
	if got.Get("Accept-Encoding") != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity", got.Get("Accept-Encoding"))
	}
}
