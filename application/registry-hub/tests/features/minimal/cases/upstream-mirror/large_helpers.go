package upstreammirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
)

func prepareLargeBlocker(ctx context.Context, run *mirrorRun, mib int) (map[string]any, error) {
	if err := setBackendEnvironment(ctx, run.environment, "REGISTRY_HUB_SOLIDIFICATION_MAX_PROTECTION_SESSIONS=1", "REGISTRY_HUB_SOLIDIFICATION_MAX_UNANCHORED_DISTRIBUTION_BYTES=2147483648", "REGISTRY_HUB_WORKERS=1"); err != nil {
		return nil, err
	}
	body := []byte(fmt.Sprintf("large-layer-blocker-%d\n", mib))
	digest := support.Digest(body)
	response, err := run.registry.Get(ctx, run.remoteRepository, "/blobs/"+digest, nil)
	if err != nil {
		return nil, err
	}
	if err := support.VerifyResponseBody(response, body, 1024); err != nil {
		return nil, err
	}
	return WaitForTaskStatus(ctx, run.environment, digest, "verified_local", 3*time.Minute)
}

func readLargeLayer(ctx context.Context, registry *support.RegistryClient, repository string, layer support.Descriptor) (float64, error) {
	started := time.Now()
	response, err := registry.Get(ctx, repository, "/blobs/"+layer.Digest, nil)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return 0, fmt.Errorf("layer GET returned %s: %s", response.Status, body)
	}
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(response.Body, int64(layer.Size)+1))
	if err != nil {
		return 0, err
	}
	if size != int64(layer.Size) || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != layer.Digest {
		return 0, fmt.Errorf("layer size/digest mismatch: bytes=%d expected=%d", size, layer.Size)
	}
	return time.Since(started).Seconds(), nil
}

func requireLargeUpstreamGETs(ctx context.Context, run *mirrorRun, digest string, expected int) error {
	methods, err := fixtureMethodsForBlob(ctx, run.environment, digest)
	if err != nil {
		return err
	}
	count := 0
	for _, method := range methods {
		if method == http.MethodGet {
			count++
		}
	}
	if count != expected {
		return fmt.Errorf("upstream layer GETs=%d expected=%d", count, expected)
	}
	return nil
}
