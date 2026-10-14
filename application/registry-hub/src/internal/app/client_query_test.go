package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"registry-hub/internal/route"
)

func TestValidateQueryUploadLifecycle(t *testing.T) {
	creation := route.Route{Endpoint: route.EndpointUpload}
	completion := route.Route{Endpoint: route.EndpointUpload, UploadID: "upload"}

	if err := validateQuery(http.MethodPost, creation, url.Values{"digest-algorithm": {"sha512"}}, 1000); err != nil {
		t.Fatalf("chunked upload creation rejected digest-algorithm: %v", err)
	}
	if err := validateQuery(http.MethodPost, creation, url.Values{"digest": {"sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}, 1000); err == nil {
		t.Fatal("upload creation accepted repeated digest")
	}
	if err := validateQuery(http.MethodPut, completion, url.Values{"_state": {"token"}, "digest": {"sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "sha512:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}, 1000); err != nil {
		t.Fatalf("upload completion rejected repeated digest: %v", err)
	}
	if err := validateQuery(http.MethodGet, completion, url.Values{"_state": {"token"}, "digest": {"sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}, 1000); err == nil {
		t.Fatal("upload status accepted a completion digest")
	}
	if err := validateQuery(http.MethodPut, completion, url.Values{"_state": {"token"}}, 1000); err == nil {
		t.Fatal("upload completion accepted a missing digest")
	}
}

func TestNormalizeTagListHonorsZeroPageSize(t *testing.T) {
	headers := make(http.Header)
	headers.Set("Link", "</v2/internal/tags/list?n=2&last=old>; rel=\"next\"")
	data, err := normalizeTagList([]byte(`{"name":"internal","tags":["a","b"]}`), "docker.io/library/busybox", url.Values{"n": {"0"}}, 2, headers, false)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Name != "docker.io/library/busybox" || len(result.Tags) != 0 {
		t.Fatalf("normalized tags = %#v, want external empty response", result)
	}
	if headers.Get("Link") != "" {
		t.Fatalf("n=0 retained Link header %q", headers.Get("Link"))
	}
}

func TestNormalizeTagListFiltersLastAndCreatesBackendLink(t *testing.T) {
	headers := make(http.Header)
	headers.Set("Link", `</v2/internal/tags/list?n=2&last=opaque>; rel="next"`)
	data, err := normalizeTagList([]byte(`{"name":"internal","tags":["z","a","c","b"]}`), "library/busybox", url.Values{"last": {"a"}}, 2, headers, false)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if got, want := result.Tags, []string{"b", "c"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("normalized tags = %#v, want %#v", got, want)
	}
	if link := headers.Get("Link"); link != `</v2/library/busybox/tags/list?last=c&n=2>; rel="next"` {
		t.Fatalf("Link = %q", link)
	}
}

func TestNormalizeTagListKeepsUpstreamPaginationSource(t *testing.T) {
	headers := make(http.Header)
	data, err := normalizeTagList([]byte(`{"name":"internal","tags":["a","b","c","d"]}`), "ghcr.io/team/image", url.Values{"n": {"3"}, "_origin": {"upstream"}}, 100, headers, false)
	if err != nil {
		t.Fatal(err)
	}
	if link := headers.Get("Link"); link != `</v2/ghcr.io/team/image/tags/list?_origin=upstream&last=c&n=3>; rel="next"` {
		t.Fatalf("Link = %q", link)
	}
	var result struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Tags) != 3 || result.Tags[2] != "c" {
		t.Fatalf("normalized tags = %#v", result.Tags)
	}
}

func TestValidateQueryAcceptsOnlyUpstreamTagPaginationOrigin(t *testing.T) {
	routed := route.Route{Endpoint: route.EndpointTags, HasUpstream: true}
	if err := validateQuery(http.MethodGet, routed, url.Values{"_origin": {"upstream"}, "last": {"a"}}, 100); err != nil {
		t.Fatalf("valid upstream pagination origin rejected: %v", err)
	}
	for _, test := range []struct {
		name   string
		routed route.Route
		query  url.Values
	}{
		{name: "unknown origin", routed: routed, query: url.Values{"_origin": {"local"}}},
		{name: "repository without upstream", routed: route.Route{Endpoint: route.EndpointTags}, query: url.Values{"_origin": {"upstream"}}},
		{name: "repeated origin", routed: routed, query: url.Values{"_origin": {"upstream", "upstream"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateQuery(http.MethodGet, test.routed, test.query, 100); err == nil {
				t.Fatal("invalid pagination origin was accepted")
			}
		})
	}
}

func TestNormalizeTagListDropsInternalLinkAfterFinalLocalPage(t *testing.T) {
	headers := make(http.Header)
	headers.Set("Link", `</v2/internal/tags/list?n=2&last=opaque>; rel="next"`)
	data, err := normalizeTagList([]byte(`{"name":"internal","tags":["last"]}`), "library/busybox", url.Values{"n": {"2"}}, 100, headers, false)
	if err != nil {
		t.Fatal(err)
	}
	if link := headers.Get("Link"); link != "" {
		t.Fatalf("final local page retained internal Link %q", link)
	}
	var result struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Name != "library/busybox" || len(result.Tags) != 1 || result.Tags[0] != "last" {
		t.Fatalf("normalized final local tags = %#v", result)
	}
}

func TestCollectUpstreamTagPagesSortsAcrossSourceOrder(t *testing.T) {
	first := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Link": []string{`</v2/team/image/tags/list?last=z&n=2>; rel="next"`}},
		Body:       io.NopCloser(strings.NewReader(`{"name":"internal","tags":["z","a"]}`)),
	}

	result, err := collectUpstreamTagPages(first, "/v2/team/image/tags/list", "registry.example", 5, 10, 1024, func(query url.Values) (*http.Response, error) {
		if query.Get("last") != "z" || query.Get("n") != "2" {
			t.Fatalf("next page query = %v", query)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"name":"internal","tags":["c","b","a"]}`)),
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var document struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(result, &document); err != nil {
		t.Fatal(err)
	}
	if got, want := document.Tags, []string{"a", "b", "c", "z"}; len(got) != len(want) {
		t.Fatalf("tags = %#v, want %#v", got, want)
	} else {
		for index := range want {
			if got[index] != want[index] {
				t.Fatalf("tags = %#v, want %#v", got, want)
			}
		}
	}
}

func TestNextTagPageRejectsCrossHostLinks(t *testing.T) {
	_, found, err := nextTagPage([]string{`<https://evil.example/v2/team/image/tags/list?last=a&n=2>; rel="next"`}, "/v2/team/image/tags/list", "registry.example", 10)
	if err == nil || found {
		t.Fatalf("cross-host Link result found=%v err=%v", found, err)
	}
}
