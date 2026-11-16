package distribution

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRequestEscapesRegistryReferenceOnce(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	client := &Client{base: &url.URL{Scheme: "https", Host: "registry.test"}}
	request, err := client.request(context.Background(), "GET", "/v2/team/image/manifests/"+url.PathEscape(digest), nil, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := "/v2/team/image/manifests/" + url.PathEscape(digest)
	if request.URL.EscapedPath() != want {
		t.Fatalf("escaped path = %q, want %q", request.URL.EscapedPath(), want)
	}
	if strings.Contains(request.URL.EscapedPath(), "%25") {
		t.Fatalf("path was encoded twice: %q", request.URL.EscapedPath())
	}
}

func TestEmptyUploadCommitDoesNotUseChunkedTransfer(t *testing.T) {
	var contentLength int64
	var transferEncoding []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		contentLength = request.ContentLength
		transferEncoding = append([]string(nil), request.TransferEncoding...)
		writer.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{base: endpoint, client: server.Client()}
	response, err := client.Do(context.Background(), http.MethodPut, "/v2/repository/blobs/uploads/session", url.Values{"digest": {"sha256:abc"}}, nil, strings.NewReader(""), 0)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("upload commit status = %d, want %d", response.StatusCode, http.StatusCreated)
	}
	if contentLength != 0 || len(transferEncoding) != 0 {
		t.Fatalf("empty request had ContentLength=%d TransferEncoding=%v, want length 0 without chunked transfer", contentLength, transferEncoding)
	}
}

func TestDistributionAPIErrorObserverUsesBoundedStatus(t *testing.T) {
	client := &Client{base: &url.URL{Scheme: "https", Host: "registry.example"}}
	client.client = &http.Client{Transport: distributionRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})}
	observed := map[string]int{}
	client.SetObserver(func(event, result string) { observed[event+":"+result]++ })
	response, err := client.Do(context.Background(), http.MethodGet, "/v2/", nil, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if observed["api_error:503"] != 1 {
		t.Fatalf("503 API error observations = %d, want 1", observed["api_error:503"])
	}
}

type distributionRoundTripper func(*http.Request) (*http.Response, error)

func (f distributionRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestHTTPAttemptObserverReceivesRequestContext(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "inventory-worker")
	client := &Client{base: &url.URL{Scheme: "https", Host: "registry.example"}}
	client.client = &http.Client{Transport: observedTransport{
		base: distributionRoundTripper(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
		}),
		client: client,
	}}
	var observed HTTPAttempt
	client.SetHTTPAttemptObserver(func(attempt HTTPAttempt) { observed = attempt })
	response, err := client.Do(ctx, http.MethodGet, "/v2/", nil, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if observed.Context == nil || observed.Context.Value(contextKey{}) != "inventory-worker" {
		t.Fatal("HTTP attempt did not retain its request context")
	}
}
