package app

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"registry-hub/internal/inventory"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
)

func (s *Service) inventoryDiscoverLocalContent(ctx context.Context, content inventory.Content) error {
	if s.registry == nil {
		return storage.ErrNotFound
	}
	internal, err := inventoryInternalName(content.Identity)
	if err != nil {
		return err
	}
	response, err := s.registry.Do(ctx, http.MethodGet, "/v2/"+internal+"/manifests/"+url.PathEscape(content.Digest), nil, manifestReadHeaders(), nil, 0)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound && confirmedMiss(response, http.MethodGet, route.EndpointManifest) {
		return storage.ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return storage.ErrUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, s.cfg.Limits.ManifestBytes+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > s.cfg.Limits.ManifestBytes || artifactDigest(content.Digest) != nil {
		return storage.ErrCorruptObject
	}
	if err := verifyInventoryManifest(content.Digest, data); err != nil {
		return err
	}
	_, err = s.inventoryContent(ctx, content.Identity, content.Digest, response.Header.Get("Content-Type"), data)
	return err
}
