package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLargeLayerFixtureDigestAndBoundedRange(t *testing.T) {
	hash := sha256.New()
	if _, err := io.Copy(hash, io.NewSectionReader(largeLayerReader{}, 0, 64<<20)); err != nil {
		t.Fatal(err)
	}
	if "sha256:"+hex.EncodeToString(hash.Sum(nil)) != largeLayerDigests[64] {
		t.Fatal("generated payload differs from manifest digest")
	}
	runtime := &fixtureRuntime{}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		request := httptest.NewRequest(method, "/v2/registry-hub-test/fixture/blobs/"+largeLayerDigests[1024], nil)
		request.Header.Set("Range", "bytes=255-258")
		response := httptest.NewRecorder()
		if !runtime.serveLargeRoutes(response, request, fixtureRequestState{host: "github.com", auth: "absent"}) || response.Code != http.StatusPartialContent {
			t.Fatalf("range route failed: status=%d", response.Code)
		}
		if method == http.MethodGet && response.Body.String() != string([]byte{255, 0, 1, 2}) {
			t.Fatal("range payload mismatch")
		}
		if method == http.MethodHead && response.Body.Len() != 0 {
			t.Fatal("HEAD emitted payload bytes")
		}
		if response.Header().Get("Content-Range") != "bytes 255-258/1073741824" {
			t.Fatal("range size differs from descriptor")
		}
	}
}
