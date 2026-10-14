package main

import (
	"bytes"
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

func (runtime *fixtureRuntime) buildDiscoveryIndex() error {
	var err error
	runtime.indexBytes, err = json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     ociIndexMediaType,
		"manifests": []any{map[string]any{
			"mediaType": "application/vnd.oci.image.manifest.v1+json",
			"digest":    runtime.manifestDigest,
			"size":      len(runtime.manifestBytes),
			"platform": map[string]any{
				"architecture": "amd64",
				"os":           "linux",
				"variant":      "v3",
				"os.version":   "fixture-1",
				"os.features":  []string{"fixture-feature"},
			},
			"annotations":          map[string]string{"org.opencontainers.image.ref.name": "fixture-child"},
			"x-fixture-descriptor": "retained",
		}},
	})
	if err != nil {
		return fmt.Errorf("fixture OCI index is invalid: %w", err)
	}
	runtime.indexDigest = "sha256:" + hex.EncodeToString(sha256Bytes(runtime.indexBytes))
	return nil
}

func (runtime *fixtureRuntime) buildBlobFixtures() error {
	multipartSeed := []byte("registry-hub-backend-s3-multipart:")
	runtime.multipartBlob = bytes.Repeat(multipartSeed, (6<<20)/len(multipartSeed)+1)
	runtime.multipartBlobDigest = "sha256:" + hex.EncodeToString(sha256Bytes(runtime.multipartBlob))
	var err error
	runtime.multipartManifestBytes, err = json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"size":      len(runtime.config),
			"digest":    runtime.configDigest,
		},
		"layers": []any{map[string]any{
			"mediaType": "application/vnd.oci.image.layer.v1.tar",
			"size":      len(runtime.multipartBlob),
			"digest":    runtime.multipartBlobDigest,
		}},
	})
	if err != nil {
		return fmt.Errorf("fixture multipart manifest is invalid: %w", err)
	}
	runtime.multipartManifestDigest = "sha256:" + hex.EncodeToString(sha256Bytes(runtime.multipartManifestBytes))
	runtime.objectSizeBlob = []byte("object-size-limit-fixture")
	runtime.objectSizeBlobDigest = "sha256:" + hex.EncodeToString(sha256Bytes(runtime.objectSizeBlob))
	runtime.objectSizeManifestBytes, err = json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"size":      len(runtime.objectSizeBlob),
			"digest":    runtime.objectSizeBlobDigest,
		},
		"layers": []any{},
	})
	if err != nil {
		return fmt.Errorf("fixture object-size-limit manifest is invalid: %w", err)
	}
	runtime.objectSizeManifestDigest = "sha256:" + hex.EncodeToString(sha256Bytes(runtime.objectSizeManifestBytes))
	runtime.rangeBlob = []byte("upstream-range-fixture")
	runtime.rangeBlobDigest = "sha256:" + hex.EncodeToString(sha256Bytes(runtime.rangeBlob))
	runtime.partialBlob = []byte("partial-upstream-range-fixture")
	runtime.partialBlobDigest = "sha256:" + hex.EncodeToString(sha256Bytes(runtime.partialBlob))
	runtime.waiterBlob = []byte("minimal-shared-object-waiter")
	runtime.waiterBlobDigest = "sha256:" + hex.EncodeToString(sha256Bytes(runtime.waiterBlob))
	return runtime.buildPartialManifest()
}
