package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

func (runtime *fixtureRuntime) serveQuotaRoutes(writer http.ResponseWriter, request *http.Request, state fixtureRequestState) bool {
	if state.host != "github.com" || state.auth != "absent" {
		return false
	}
	config := []byte(`{"architecture":"amd64","os":"linux","quotaFixture":true}`)
	layer := []byte("registry-hub quota convergence layer\n")
	digest := func(body []byte) string { sum := sha256.Sum256(body); return "sha256:" + hex.EncodeToString(sum[:]) }
	descriptor := func(body []byte, media string) map[string]any {
		return map[string]any{"mediaType": media, "digest": digest(body), "size": len(body)}
	}
	configuration := descriptor(config, "application/vnd.oci.image.config.v1+json")
	configuration["data"] = base64.StdEncoding.EncodeToString(config)
	manifest, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json", "config": configuration, "layers": []any{descriptor(layer, "application/vnd.oci.image.layer.v1.tar")}})
	path := request.URL.Path
	switch {
	case strings.HasPrefix(path, githubManifestPrefix+"quota-convergence-") || path == githubManifestPrefix+digest(manifest):
		writeManifestRepresentation(writer, manifest, "application/vnd.oci.image.manifest.v1+json", digest(manifest))
	case path == "/v2/registry-hub-test/fixture/blobs/"+digest(config):
		writeBlob(writer, config, digest(config))
	case path == "/v2/registry-hub-test/fixture/blobs/"+digest(layer):
		writeBlob(writer, layer, digest(layer))
	default:
		return false
	}
	return true
}
