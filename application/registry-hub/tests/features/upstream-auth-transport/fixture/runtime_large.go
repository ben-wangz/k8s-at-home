package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

var largeLayerDigests = map[int]string{
	64:   "sha256:281e519df3077b557c6b03f5da83c4e8d397219259615dd7c3308f89cae8f2a6",
	256:  "sha256:486cc817b95d853d3c357ff283b204c0144bd255e73fe2deb1389493b257e3c0",
	1024: "sha256:2c06ade942ee3f17a048dd1064b2fab046a4bb95386d8bb41b68dc6711ac2af3",
}

type largeLayerReader struct{}

func (largeLayerReader) ReadAt(body []byte, offset int64) (int, error) {
	for index := range body {
		body[index] = byte(offset + int64(index))
	}
	return len(body), nil
}

func largeFixture(mib int) ([]byte, string, []byte, string) {
	digest := func(body []byte) string {
		sum := sha256.Sum256(body)
		return "sha256:" + hex.EncodeToString(sum[:])
	}
	config := []byte(fmt.Sprintf(`{"architecture":"amd64","os":"linux","largeLayerMiB":%d}`, mib))
	blocker := []byte(fmt.Sprintf("large-layer-blocker-%d\n", mib))
	manifest, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": digest(config), "size": len(config), "data": base64.StdEncoding.EncodeToString(config)},
		"layers": []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": largeLayerDigests[mib], "size": int64(mib) << 20}},
	})
	return manifest, digest(manifest), blocker, digest(blocker)
}

func (runtime *fixtureRuntime) serveLargeRoutes(writer http.ResponseWriter, request *http.Request, state fixtureRequestState) bool {
	if state.host != "github.com" || state.auth != "absent" {
		return false
	}
	for _, mib := range []int{64, 256, 1024} {
		manifest, manifestID, blocker, blockerID := largeFixture(mib)
		switch request.URL.Path {
		case githubManifestPrefix + fmt.Sprintf("large-layer-%d", mib), githubManifestPrefix + manifestID:
			writeManifestRepresentation(writer, manifest, "application/vnd.oci.image.manifest.v1+json", manifestID)
		case "/v2/registry-hub-test/fixture/blobs/" + blockerID:
			writeBlob(writer, blocker, blockerID)
		case "/v2/registry-hub-test/fixture/blobs/" + largeLayerDigests[mib]:
			writer.Header().Set("Content-Type", "application/octet-stream")
			writer.Header().Set("Docker-Content-Digest", largeLayerDigests[mib])
			writer.Header().Set("ETag", `"`+largeLayerDigests[mib]+`"`)
			http.ServeContent(writer, request, "layer", time.Unix(1, 0), io.NewSectionReader(largeLayerReader{}, 0, int64(mib)<<20))
		default:
			continue
		}
		return true
	}
	return false
}
