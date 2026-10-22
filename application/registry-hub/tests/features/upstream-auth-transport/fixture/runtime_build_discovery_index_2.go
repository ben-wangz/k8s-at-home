package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

func (runtime *fixtureRuntime) buildDiscoveryIndex() error {
	runtime.discoveryConfig = []byte("registry-hub-discovery-config")
	runtime.discoveryConfigDigest = "sha256:" + hex.EncodeToString(sha256Bytes(runtime.discoveryConfig))
	var err error
	runtime.discoveryManifestBytes, err = json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"size":      len(runtime.discoveryConfig),
			"digest":    runtime.discoveryConfigDigest,
		},
		"layers": []any{},
	})
	if err != nil {
		return fmt.Errorf("fixture discovery manifest is invalid: %w", err)
	}
	runtime.discoveryManifestDigest = "sha256:" + hex.EncodeToString(sha256Bytes(runtime.discoveryManifestBytes))
	runtime.indexBytes, err = json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     ociIndexMediaType,
		"manifests": []any{map[string]any{
			"mediaType": "application/vnd.oci.image.manifest.v1+json",
			"digest":    runtime.discoveryManifestDigest,
			"size":      len(runtime.discoveryManifestBytes),
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
