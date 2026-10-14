package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

func buildNestedDiscoveryFixture(config []byte) (fixtureManifest, map[string]fixtureManifest, error) {
	configDigest := "sha256:" + hex.EncodeToString(sha256Bytes(config))
	platforms := []struct {
		name         string
		os           string
		architecture string
		variant      string
		osVersion    string
	}{
		{name: "linux-amd64", os: "linux", architecture: "amd64", variant: "v3"},
		{name: "linux-arm64", os: "linux", architecture: "arm64", variant: "v8"},
		{name: "windows-amd64", os: "windows", architecture: "amd64", osVersion: "10.0.20348.0"},
		{name: "freebsd-riscv64", os: "freebsd", architecture: "riscv64"},
	}
	objects := make(map[string]fixtureManifest, len(platforms)+2)
	linuxDescriptors := make([]any, 0, 2)
	rootDescriptors := make([]any, 0, 2)
	for _, platform := range platforms {
		manifestBody, err := json.Marshal(map[string]any{
			"schemaVersion": 2,
			"mediaType":     "application/vnd.oci.image.manifest.v1+json",
			"annotations":   map[string]string{"org.registry-hub.fixture.platform": platform.name},
			"config": map[string]any{
				"mediaType": "application/vnd.oci.image.config.v1+json",
				"size":      len(config),
				"digest":    configDigest,
			},
			"layers": []any{},
		})
		if err != nil {
			return fixtureManifest{}, nil, err
		}
		manifest := fixtureManifest{Body: manifestBody, MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: "sha256:" + hex.EncodeToString(sha256Bytes(manifestBody))}
		objects[manifest.Digest] = manifest
		platformFields := map[string]any{"os": platform.os, "architecture": platform.architecture}
		if platform.variant != "" {
			platformFields["variant"] = platform.variant
		}
		if platform.osVersion != "" {
			platformFields["os.version"] = platform.osVersion
		}
		descriptor := map[string]any{
			"mediaType": manifest.MediaType,
			"size":      len(manifest.Body),
			"digest":    manifest.Digest,
			"platform":  platformFields,
			"annotations": map[string]string{
				"org.registry-hub.fixture.platform": platform.name,
			},
		}
		if platform.os == "linux" {
			linuxDescriptors = append(linuxDescriptors, descriptor)
		} else {
			rootDescriptors = append(rootDescriptors, descriptor)
		}
	}
	leafBody, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     ociIndexMediaType,
		"annotations":   map[string]string{"org.registry-hub.fixture.group": "linux-leaf"},
		"manifests":     linuxDescriptors,
	})
	if err != nil {
		return fixtureManifest{}, nil, err
	}
	leaf := fixtureManifest{Body: leafBody, MediaType: ociIndexMediaType, Digest: "sha256:" + hex.EncodeToString(sha256Bytes(leafBody))}
	objects[leaf.Digest] = leaf
	nestedBody, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     ociIndexMediaType,
		"annotations":   map[string]string{"org.registry-hub.fixture.group": "linux"},
		"manifests": []any{map[string]any{
			"mediaType": leaf.MediaType,
			"size":      len(leaf.Body),
			"digest":    leaf.Digest,
		}},
	})
	if err != nil {
		return fixtureManifest{}, nil, err
	}
	nested := fixtureManifest{Body: nestedBody, MediaType: ociIndexMediaType, Digest: "sha256:" + hex.EncodeToString(sha256Bytes(nestedBody))}
	objects[nested.Digest] = nested
	rootDescriptors = append([]any{map[string]any{
		"mediaType": nested.MediaType,
		"size":      len(nested.Body),
		"digest":    nested.Digest,
		"urls":      []string{"https://github.com/v2/registry-hub-test/fixture/url-must-not-fetch"},
		"annotations": map[string]string{
			"org.registry-hub.fixture.group": "linux",
		},
	}}, rootDescriptors...)
	rootBody, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     ociIndexMediaType,
		"annotations":   map[string]string{"org.registry-hub.fixture.root": "all-platforms"},
		"manifests":     rootDescriptors,
	})
	if err != nil {
		return fixtureManifest{}, nil, err
	}
	return fixtureManifest{Body: rootBody, MediaType: ociIndexMediaType, Digest: "sha256:" + hex.EncodeToString(sha256Bytes(rootBody))}, objects, nil
}

func buildClosureDepthFixture() (fixtureManifest, map[string]fixtureManifest, error) {
	objects := make(map[string]fixtureManifest, 66)
	var child fixtureManifest
	for depth := 65; depth >= 0; depth-- {
		manifests := []any{}
		if child.Digest != "" {
			manifests = append(manifests, map[string]any{
				"mediaType": child.MediaType,
				"size":      len(child.Body),
				"digest":    child.Digest,
			})
		}
		body, err := json.Marshal(map[string]any{
			"schemaVersion": 2,
			"mediaType":     ociIndexMediaType,
			"annotations":   map[string]string{"org.registry-hub.fixture.depth": fmt.Sprint(depth)},
			"manifests":     manifests,
		})
		if err != nil {
			return fixtureManifest{}, nil, err
		}
		child = fixtureManifest{Body: body, MediaType: ociIndexMediaType, Digest: "sha256:" + hex.EncodeToString(sha256Bytes(body))}
		objects[child.Digest] = child
	}
	return child, objects, nil
}

func buildInlineDataFailureFixture(data any, size int, digest string) (fixtureManifest, error) {
	body, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"size":      size,
			"digest":    digest,
			"data":      data,
		},
		"layers": []any{},
	})
	if err != nil {
		return fixtureManifest{}, err
	}
	return fixtureManifest{
		Body:      body,
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    "sha256:" + hex.EncodeToString(sha256Bytes(body)),
	}, nil
}

func buildInlineManifest(config, layer, subject []byte, urls []string) (inlineManifestFixture, error) {
	configDigest := "sha256:" + hex.EncodeToString(sha256Bytes(config))
	layerDigest := "sha256:" + hex.EncodeToString(sha256Bytes(layer))
	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"artifactType":  "application/vnd.registry-hub.test.inline",
		"annotations": map[string]string{
			"org.opencontainers.image.title": "inline fixture",
		},
		"x-fixture-manifest": map[string]any{"retained": true},
		"config": map[string]any{
			"mediaType":            "application/vnd.oci.image.config.v1+json",
			"size":                 len(config),
			"digest":               configDigest,
			"data":                 base64.StdEncoding.EncodeToString(config),
			"annotations":          map[string]string{"org.opencontainers.image.title": "inline config"},
			"x-fixture-descriptor": "config-retained",
		},
		"layers": []any{map[string]any{
			"mediaType":            "application/vnd.oci.image.layer.v1.tar",
			"size":                 len(layer),
			"digest":               layerDigest,
			"urls":                 urls,
			"annotations":          map[string]string{"org.opencontainers.image.title": "url-bearing layer"},
			"artifactType":         "application/vnd.registry-hub.test.layer",
			"x-fixture-descriptor": "layer-retained",
		}},
	}
	subjectDigest := ""
	if subject != nil {
		subjectDigest = "sha256:" + hex.EncodeToString(sha256Bytes(subject))
		manifest["subject"] = map[string]any{
			"mediaType": "application/vnd.oci.image.manifest.v1+json",
			"size":      len(subject),
			"digest":    subjectDigest,
			"annotations": map[string]string{
				"org.opencontainers.image.title": "unfetched subject",
			},
			"x-fixture-descriptor": "subject-retained",
		}
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return inlineManifestFixture{}, err
	}
	return inlineManifestFixture{
		Manifest: body, ManifestDigest: "sha256:" + hex.EncodeToString(sha256Bytes(body)),
		Config: config, ConfigDigest: configDigest, Layer: layer, LayerDigest: layerDigest,
	}, nil
}

func sha256Bytes(value []byte) []byte {
	digest := sha256.Sum256(value)
	return digest[:]
}
