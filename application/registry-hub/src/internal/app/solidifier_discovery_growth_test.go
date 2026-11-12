package app

import (
	"fmt"
	"registry-hub/internal/artifact"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
	"time"
)

func TestDiscoveryMeasuresDuplicateWakeMetadataGrowth(t *testing.T) {
	for _, platforms := range []int{4, 16, 64} {
		t.Run(fmt.Sprint(platforms), func(t *testing.T) {
			base, err := filesystem.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer base.Close()
			store := &nestedMetadataStore{Store: base, calls: map[string]int{}, writes: map[string]int{}}
			f := newStagedAcceptanceFixtureWithStore(t, 1<<20, store)
			f.processor.cfg.Limits.ClosureObjects = 1000
			children := make([]artifact.Descriptor, 0, platforms)
			for n := range platforms {
				body := []byte(fmt.Sprintf("uncached-platform-%d", n))
				blob := artifact.Descriptor{Digest: artifact.CalculateSHA256(body), Size: int64(len(body)), MediaType: testOCIConfigMediaType}
				children = append(children, f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &blob, Layers: []artifact.Descriptor{}}))
			}
			index := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociIndexMediaType, Manifests: &children})
			root := f.root("metadata-growth", index)
			for _, phase := range []string{"cold", "duplicate-wake-1", "duplicate-wake-2"} {
				store.calls, store.writes = map[string]int{}, map[string]int{}
				started := time.Now()
				if err := f.step(root.ID); err == nil {
					t.Fatal("uncached platforms unexpectedly completed")
				}
				edgeWrites := 0
				for key, count := range store.writes {
					if strings.Contains(key, "/edges/") {
						edgeWrites += count
					}
				}
				if phase != "cold" && edgeWrites != 0 {
					t.Fatalf("snapshot reuse rewrote %d immutable edges", edgeWrites)
				}
				t.Logf("platforms=%d phase=%s seconds=%.6f calls=%v edge_puts=%d", platforms, phase, time.Since(started).Seconds(), store.calls, edgeWrites)
			}
		})
	}
}
