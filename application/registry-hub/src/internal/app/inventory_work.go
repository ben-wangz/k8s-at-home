package app

import (
	"context"
	"net/http"
	"net/url"
	"registry-hub/internal/inventory"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
)

func inventoryWorkActive(work inventory.Work) bool {
	state, _ := inventory.Execution([]inventory.Work{work})
	return state != "idle" && state != "not_scheduled"
}

func (s *Service) inventoryTagPublished(ctx context.Context, image inventory.Image) (bool, error) {
	internal, err := inventoryInternalName(image.Identity)
	if err != nil {
		return false, err
	}
	response, err := s.registry.Do(ctx, http.MethodHead, "/v2/"+internal+"/manifests/"+url.PathEscape(image.Reference), nil, manifestReadHeaders(), nil, 0)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound && confirmedMiss(response, http.MethodHead, route.EndpointManifest) {
		return false, nil
	}
	if response.StatusCode != http.StatusOK {
		return false, storage.ErrUnavailable
	}
	return response.Header.Get("Docker-Content-Digest") == image.ResolvedDigest && response.ContentLength >= 0, nil
}
