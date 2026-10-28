package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"registry-hub/internal/inventory"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func (s *Service) inventoryRegistryJSON(ctx context.Context, path string, query url.Values, value any) error {
	response, err := s.registry.Do(ctx, http.MethodGet, path, query, manifestReadHeaders(), nil, 0)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return storage.ErrUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, s.cfg.Limits.ManifestBytes+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > s.cfg.Limits.ManifestBytes {
		return storage.ErrCorruptObject
	}
	if err := json.Unmarshal(data, value); err != nil {
		return storage.ErrCorruptObject
	}
	return nil
}

func (s *Service) scanInventoryCatalog(ctx context.Context, state *inventoryScan) error {
	if s.registry == nil {
		state.Phase++
		state.Reasons = appendReason(state.Reasons, "distribution_catalog_unavailable")
		return nil
	}
	if state.CatalogRepository == "" && len(state.Repositories) == 0 {
		var page struct {
			Repositories []string `json:"repositories"`
		}
		if err := s.inventoryRegistryJSON(ctx, "/v2/_catalog", url.Values{"n": {"100"}, "last": {state.Token}}, &page); err != nil {
			return err
		}
		if len(page.Repositories) == 0 {
			state.Phase++
			state.Token = ""
			return nil
		}
		state.Repositories = page.Repositories
		state.Token = page.Repositories[len(page.Repositories)-1]
	}
	if state.CatalogRepository == "" {
		state.CatalogRepository = state.Repositories[0]
		state.Repositories = state.Repositories[1:]
		state.CatalogLast = ""
	}
	name, ok := route.DecodeInternalName(state.CatalogRepository)
	authority, repository := "", state.CatalogRepository
	if ok {
		authority, repository, _ = strings.Cut(name, "/")
	}
	var tags struct {
		Tags []string `json:"tags"`
	}
	if err := s.inventoryRegistryJSON(ctx, "/v2/"+state.CatalogRepository+"/tags/list", url.Values{"n": {"100"}, "last": {state.CatalogLast}}, &tags); err != nil {
		return err
	}
	for _, tag := range tags.Tags {
		if strings.HasSuffix(tag, ".referrers") {
			continue
		}
		if err := s.inventoryLocalTag(ctx, inventoryIdentity(authority, repository, tag), state.CatalogRepository); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
	}
	if len(tags.Tags) < 100 {
		state.CatalogRepository = ""
		state.CatalogLast = ""
	} else {
		state.CatalogLast = tags.Tags[len(tags.Tags)-1]
	}
	return nil
}

func (s *Service) inventoryLocalTag(ctx context.Context, identity inventory.Identity, internal string) error {
	response, err := s.registry.Do(ctx, http.MethodGet, "/v2/"+internal+"/manifests/"+url.PathEscape(identity.Reference), nil, manifestReadHeaders(), nil, 0)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return storage.ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return storage.ErrUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, s.cfg.Limits.ManifestBytes+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > s.cfg.Limits.ManifestBytes {
		return storage.ErrCorruptObject
	}
	digest := response.Header.Get("Docker-Content-Digest")
	if artifactDigest(digest) != nil {
		return storage.ErrCorruptObject
	}
	if err := verifyInventoryManifest(digest, data); err != nil {
		return err
	}
	content, err := s.inventoryContent(ctx, identity, digest, response.Header.Get("Content-Type"), data)
	if err != nil {
		return err
	}
	generation, err := s.generation(ctx, identity.Authority, identity.Repository, identity.Reference)
	if err != nil {
		return err
	}
	_, err = s.inventoryBinding(ctx, identity, digest, "distribution", generation.Generation, time.Now().UTC(), false)
	if err != nil {
		return err
	}
	_, err = s.inventoryBinding(ctx, inventoryIdentity(identity.Authority, identity.Repository, digest), digest, "distribution", content.Generation, time.Now().UTC(), false)
	return err
}
