package main

import (
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
)

const githubManifestPrefix = "/v2/registry-hub-test/fixture/manifests/"

type fixtureRuntime struct {
	tlsConfig                 *tls.Config
	dockerCredential          credential
	ghcrCredential            credential
	config                    []byte
	configDigest              string
	discoveryConfig           []byte
	discoveryConfigDigest     string
	discoveryManifestBytes    []byte
	discoveryManifestDigest   string
	multipartBlob             []byte
	multipartBlobDigest       string
	multipartManifestBytes    []byte
	multipartManifestDigest   string
	objectSizeBlob            []byte
	objectSizeBlobDigest      string
	objectSizeManifestBytes   []byte
	objectSizeManifestDigest  string
	rangeBlob                 []byte
	rangeBlobDigest           string
	replacementReference      string
	inlineLayer               []byte
	inlineManifest            inlineManifestFixture
	missingURLManifest        inlineManifestFixture
	gcCancellationManifest    inlineManifestFixture
	invalidInlineManifest     fixtureManifest
	invalidInlineSize         fixtureManifest
	invalidInlineDigest       fixtureManifest
	invalidInlineType         fixtureManifest
	manifestBytes             []byte
	manifestDigest            string
	backgroundRetryBlob       []byte
	backgroundRetryBlobDigest string
	backgroundRetryManifest   []byte
	backgroundRetryManifestID string
	partialBlob               []byte
	partialBlobDigest         string
	waiterBlob                []byte
	waiterBlobDigest          string
	partialManifestBytes      []byte
	partialManifestDigest     string
	indexBytes                []byte
	indexDigest               string
	nestedConfig              []byte
	nestedConfigDigest        string
	nestedRoot                fixtureManifest
	depthRoot                 fixtureManifest
	nestedObjects             map[string]fixtureManifest
	sharedSizeConflict        fixtureManifest
	sharedMediaConflict       fixtureManifest
	resumeByReference         map[string]*resumeFixture
	resumeByDigest            map[string]*resumeBlob
	dockerListBytes           []byte
	dockerListDigest          string
	retryAfterRequests        int32
	backgroundRetryRequests   int32
	requestDeadlineRequests   int32
	tokenRetryRequests        int32
}

type fixtureRequestState struct {
	host    string
	auth    string
	scope   string
	service string
}

func main() {
	runtime, err := newFixtureRuntime()
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})
	mux.Handle("/", runtime.registryHandler())
	fixtureHandler := tunnelHandler(mux, runtime.tlsConfig)
	log.Fatal(http.ListenAndServe(":8080", fixtureHandler))
}

func newFixtureRuntime() (*fixtureRuntime, error) {
	certificate, err := tls.LoadX509KeyPair("/upstream-tls/tls.crt", "/upstream-tls/tls.key")
	if err != nil {
		return nil, fmt.Errorf("fixture TLS certificate is invalid: %w", err)
	}
	var credentials map[string]credential
	if err := json.Unmarshal([]byte(os.Getenv("REGISTRY_HUB_UPSTREAM_AUTH_CONFIG")), &credentials); err != nil {
		return nil, fmt.Errorf("fixture credentials are invalid: %w", err)
	}
	dockerCredential := credentials["docker.io"]
	if dockerCredential.Type != "basic" || dockerCredential.Username == "" || dockerCredential.Password == "" {
		return nil, fmt.Errorf("fixture credentials are incomplete")
	}
	ghcrCredential := credentials["ghcr.io"]
	if ghcrCredential.Type != "bearer" || ghcrCredential.Token == "" {
		return nil, fmt.Errorf("fixture Bearer credential is incomplete")
	}
	runtime := &fixtureRuntime{
		tlsConfig:            &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12},
		dockerCredential:     dockerCredential,
		ghcrCredential:       ghcrCredential,
		config:               []byte("{}"),
		replacementReference: "sha256:" + strings.Repeat("0", 64),
	}
	if err := runtime.buildCoreFixtures(); err != nil {
		return nil, err
	}
	if err := runtime.buildBlobFixtures(); err != nil {
		return nil, err
	}
	if err := runtime.buildResumableFixtures(); err != nil {
		return nil, err
	}
	if err := runtime.buildInlineFixtures(); err != nil {
		return nil, err
	}
	if err := runtime.buildDiscoveryFixtures(); err != nil {
		return nil, err
	}
	return runtime, nil
}

func (runtime *fixtureRuntime) buildCoreFixtures() error {
	runtime.configDigest = "sha256:" + hex.EncodeToString(sha256Bytes(runtime.config))
	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"size":      len(runtime.config),
			"digest":    runtime.configDigest,
		},
		"layers": []any{},
	}
	var err error
	runtime.manifestBytes, err = json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("fixture manifest is invalid: %w", err)
	}
	runtime.manifestDigest = "sha256:" + hex.EncodeToString(sha256Bytes(runtime.manifestBytes))
	runtime.backgroundRetryBlob = []byte("upstream-background-retry-config")
	runtime.backgroundRetryBlobDigest = "sha256:" + hex.EncodeToString(sha256Bytes(runtime.backgroundRetryBlob))
	runtime.backgroundRetryManifest, err = json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"size":      len(runtime.backgroundRetryBlob),
			"digest":    runtime.backgroundRetryBlobDigest,
		},
		"layers": []any{},
	})
	if err != nil {
		return fmt.Errorf("fixture background-retry manifest is invalid: %w", err)
	}
	runtime.backgroundRetryManifestID = "sha256:" + hex.EncodeToString(sha256Bytes(runtime.backgroundRetryManifest))
	return runtime.buildDiscoveryIndex()
}
