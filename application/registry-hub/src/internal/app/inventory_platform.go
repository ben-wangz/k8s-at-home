package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
)

func (s *Service) inventoryConfigPlatform(ctx context.Context, identity inventory.Identity, config *artifact.Descriptor) *inventory.Platform {
	if config == nil || config.Size > 1<<20 {
		return nil
	}
	inspection, err := s.cache.InspectAny(ctx, identity.Authority, identity.Repository, config.Digest, config.MediaType)
	var body []byte
	if err == nil && inspection.Present {
		body, err = s.cache.ReadManifestForInventory(ctx, inspection.CachedRepresentation, 1<<20)
	} else {
		body, err = s.inventoryLocalConfig(ctx, identity, *config)
	}
	if err != nil {
		return nil
	}
	var platform inventory.Platform
	if json.Unmarshal(body, &platform) != nil || platform.OS == "" || platform.OS == "unknown" || platform.Architecture == "" || platform.Architecture == "unknown" {
		return nil
	}
	return &platform
}

func (s *Service) inventoryLocalConfig(ctx context.Context, identity inventory.Identity, config artifact.Descriptor) ([]byte, error) {
	if s.registry == nil {
		return nil, io.EOF
	}
	internal, err := inventoryInternalName(identity)
	if err != nil {
		return nil, err
	}
	response, err := s.registry.Do(ctx, http.MethodGet, "/v2/"+internal+"/blobs/"+config.Digest, nil, nil, nil, 0)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, io.EOF
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != config.Size || len(body) > 1<<20 {
		return nil, io.ErrUnexpectedEOF
	}
	if err := artifact.VerifyDigest(config.Digest, body); err != nil {
		return nil, err
	}
	return body, nil
}
