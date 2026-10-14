package main

import (
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

type credential struct {
	Type     string `json:"type"`
	Username string `json:"username"`
	Password string `json:"password"`
	Token    string `json:"token"`
}

const (
	ociIndexMediaType   = "application/vnd.oci.image.index.v1+json"
	dockerListMediaType = "application/vnd.docker.distribution.manifest.list.v2+json"
	dockerListBody      = `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.list.v2+json","manifests":[]}`
)

func serveConnect(writer http.ResponseWriter, request *http.Request, handler http.Handler, tlsConfig *tls.Config) {
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	connection, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		connection.Close()
		return
	}
	if err := buffered.Flush(); err != nil {
		connection.Close()
		return
	}
	listener := tls.NewListener(&singleConnListener{connection: connection}, tlsConfig)
	server := &http.Server{Handler: handler}
	if err := server.Serve(listener); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("TLS proxy tunnel failed: %v", err)
	}
}

type singleConnListener struct {
	connection net.Conn
	accepted   bool
}

func (listener *singleConnListener) Accept() (net.Conn, error) {
	if listener.accepted {
		return nil, net.ErrClosed
	}
	listener.accepted = true
	return listener.connection, nil
}

func (listener *singleConnListener) Close() error { return nil }

func (listener *singleConnListener) Addr() net.Addr { return listener.connection.LocalAddr() }

func requestAuthState(request *http.Request, host string, basic, bearer credential) string {
	value := request.Header.Get("Authorization")
	if username, password, ok := request.BasicAuth(); ok {
		if username == basic.Username && password == basic.Password && (host == "registry-1.docker.io" || host == "auth.docker.io") {
			return "basic-matched"
		}
		return "unexpected"
	}
	if value != "" {
		if value == "Bearer challenge-fixture-token" && host == "registry-1.docker.io" {
			return "bearer-matched"
		}
		if value == "Bearer "+bearer.Token && host == "ghcr.io" {
			return "bearer-matched"
		}
		return "unexpected"
	}
	return "absent"
}

func writeManifest(writer http.ResponseWriter, body []byte, digest string) {
	writeManifestRepresentation(writer, body, "application/vnd.oci.image.manifest.v1+json", digest)
}

func writeManifestWithoutDigest(writer http.ResponseWriter, body []byte) {
	writer.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	writer.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(body)
}

func writeManifestRepresentation(writer http.ResponseWriter, body []byte, mediaType, digest string) {
	writer.Header().Set("Content-Type", mediaType)
	writer.Header().Set("Docker-Content-Digest", digest)
	writer.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(body)
}

func writeBlob(writer http.ResponseWriter, body []byte, digest string) {
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Docker-Content-Digest", digest)
	writer.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(body)
}

func writeDelayedBlob(writer http.ResponseWriter, request *http.Request, body []byte, digest string, delay time.Duration) {
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Docker-Content-Digest", digest)
	writer.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(body[:1])
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-request.Context().Done():
		return
	case <-timer.C:
	}
	_, _ = writer.Write(body[1:])
}

type inlineManifestFixture struct {
	Manifest       []byte
	ManifestDigest string
	Config         []byte
	ConfigDigest   string
	Layer          []byte
	LayerDigest    string
}

type fixtureManifest struct {
	Body      []byte
	MediaType string
	Digest    string
}

func nestedManifestPath(path string, objects map[string]fixtureManifest) bool {
	const prefix = "/v2/registry-hub-test/fixture/manifests/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	_, ok := objects[strings.TrimPrefix(path, prefix)]
	return ok
}

func buildSharedDigestConflictIndex(manifest []byte, digest, conflict string) (fixtureManifest, error) {
	first := map[string]any{
		"mediaType": "application/vnd.oci.image.manifest.v1+json",
		"size":      len(manifest),
		"digest":    digest,
	}
	second := map[string]any{
		"mediaType": "application/vnd.oci.image.manifest.v1+json",
		"size":      len(manifest),
		"digest":    digest,
	}
	switch conflict {
	case "size":
		second["size"] = len(manifest) + 1
	case "mediaType":
		second["mediaType"] = "application/vnd.docker.distribution.manifest.v2+json"
	default:
		return fixtureManifest{}, fmt.Errorf("unsupported shared digest conflict %q", conflict)
	}
	body, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     ociIndexMediaType,
		"annotations":   map[string]string{"org.registry-hub.fixture.conflict": conflict},
		"manifests":     []any{first, second},
	})
	if err != nil {
		return fixtureManifest{}, err
	}
	return fixtureManifest{Body: body, MediaType: ociIndexMediaType, Digest: "sha256:" + hex.EncodeToString(sha256Bytes(body))}, nil
}
