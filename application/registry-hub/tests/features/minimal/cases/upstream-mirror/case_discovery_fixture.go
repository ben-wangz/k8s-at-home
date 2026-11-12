package upstreammirror

import (
	"encoding/json"
	"fmt"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
)

const discoveryConfigBody = "registry-hub-discovery-config"

func discoveryIndexFixture() ([]byte, []byte, error) {
	config := []byte(discoveryConfigBody)
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     support.OCIManifestMediaType,
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"size":      len(config),
			"digest":    support.Digest(config),
		},
		"layers": []any{},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal discovery child manifest: %w", err)
	}
	index, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     support.OCIIndexMediaType,
		"manifests": []any{map[string]any{
			"mediaType": support.OCIManifestMediaType,
			"digest":    support.Digest(manifest),
			"size":      len(manifest),
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
		return nil, nil, fmt.Errorf("marshal discovery OCI index: %w", err)
	}
	return manifest, index, nil
}
