package upstreamauthtransport

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

const (
	relayRepository = "ghcr.nju.edu.cn/oras-project/oras"
	relayTag        = "v1.2.3"
	relayDigest     = "sha256:63266a046d1cf5ebebb1461733ed7548148f122e0d422096e177cfa70b521cb1"
)

func verifyProxyRelay(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient) (map[string]any, error) {
	body, headers, err := support.ReadManifest(ctx, registry, relayRepository, relayTag, support.OCIIndexMediaType)
	if err != nil {
		return nil, fmt.Errorf("proxied NJU mirror manifest request failed: %w", err)
	}
	if !strings.Contains(headers.Get("Content-Type"), support.OCIIndexMediaType) {
		return nil, fmt.Errorf("proxied NJU mirror returned Content-Type %q", headers.Get("Content-Type"))
	}
	actualDigest := support.Digest(body)
	if actualDigest != relayDigest || headers.Get("Docker-Content-Digest") != relayDigest {
		return nil, fmt.Errorf("proxied NJU mirror digest body=%q header=%q, want %q", actualDigest, headers.Get("Docker-Content-Digest"), relayDigest)
	}
	var index support.Manifest
	if err := json.Unmarshal(body, &index); err != nil || index.SchemaVersion != 2 || index.MediaType != support.OCIIndexMediaType {
		return nil, fmt.Errorf("proxied NJU mirror response is not a valid OCI image index")
	}
	if err := waitForDiscoveredTask(ctx, environment, "ghcr.nju.edu.cn", "oras-project/oras", relayTag); err != nil {
		return nil, fmt.Errorf("proxied NJU mirror manifest discovery did not settle: %w", err)
	}
	return map[string]any{
		"authority": "ghcr.nju.edu.cn",
		"reference": relayTag,
		"digest":    actualDigest,
		"mediaType": support.OCIIndexMediaType,
	}, nil
}

func validateProxyRelayLogs(logs string) error {
	if !strings.Contains(logs, "relay connect host=ghcr.nju.edu.cn:443 status=connected") {
		return fmt.Errorf("proxy fixture did not record a successful connection to the allowed NJU mirror")
	}
	return nil
}
