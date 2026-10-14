package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/artifact"
	"registry-hub/internal/route"
)

func (s *Service) readDigestMarkers(ctx context.Context, key string, routed route.Route, digest string) ([]referrerFallbackMarker, error) {
	object, err := s.store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer object.Body.Close()
	data, err := io.ReadAll(io.LimitReader(object.Body, 4<<20+1))
	var markers []referrerFallbackMarker
	if err != nil || len(data) > 4<<20 || json.Unmarshal(data, &markers) != nil || len(markers) > 4096 {
		return nil, invalidReferrer("fallback digest protection state is invalid")
	}
	authority, repository := routeStorageIdentity(routed)
	for _, marker := range markers {
		if marker.SchemaVersion != 1 || marker.Authority != authority || marker.Repository != repository || marker.Digest != digest || !route.ValidDigest(marker.Subject) || marker.Tag != referrerFallbackTag(marker.Subject) {
			return nil, invalidReferrer("fallback digest protection entry is invalid")
		}
	}
	return markers, nil
}

func (s *Service) readMarker(ctx context.Context, key string, routed route.Route, tag string) (referrerFallbackMarker, error) {
	object, err := s.store.Get(ctx, key)
	if err != nil {
		return referrerFallbackMarker{}, err
	}
	defer object.Body.Close()
	data, err := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
	var marker referrerFallbackMarker
	authority, repository := routeStorageIdentity(routed)
	if err != nil || len(data) > 1<<20 || json.Unmarshal(data, &marker) != nil || marker.SchemaVersion != 1 || marker.Authority != authority || marker.Repository != repository || !route.ValidDigest(marker.Subject) || marker.Tag != referrerFallbackTag(marker.Subject) || !route.ValidDigest(marker.Digest) || tag != "" && marker.Tag != tag {
		return referrerFallbackMarker{}, invalidReferrer("fallback referrer protection state is invalid")
	}
	return marker, nil
}

func fallbackTagMarkerKey(routed route.Route, tag string) string {
	authority, repository := routeStorageIdentity(routed)
	return "system/referrers/protected-tags/" + objectLeaseKey(authority, repository, tag) + ".json"
}

func fallbackDigestMarkerKey(routed route.Route, digest string) string {
	authority, repository := routeStorageIdentity(routed)
	return "system/referrers/protected-digests/" + objectLeaseKey(authority, repository, digest) + ".json"
}

func routeStorageIdentity(routed route.Route) (string, string) {
	if routed.HasUpstream {
		return routed.Upstream, routed.RemoteRepo
	}
	return "", routed.Repository
}

func taskRoute(authority, repository string) (route.Route, error) {
	logical := authority + "/" + repository
	internal, err := route.InternalName(logical)
	if err != nil {
		return route.Route{}, err
	}
	return route.Route{Repository: logical, Internal: internal, Upstream: authority, RemoteRepo: repository, HasUpstream: true}, nil
}

func readCachedManifest(ctx context.Context, cache *artifact.Cache, authority, repository, digest string, limit int64) ([]byte, string, error) {
	object, err := cache.GetAnyRetained(ctx, authority, repository, digest)
	if err != nil {
		return nil, "", err
	}
	defer object.Body.Close()
	data, err := io.ReadAll(io.LimitReader(object.Body, limit+1))
	if err != nil || int64(len(data)) > limit || int64(len(data)) != object.Size || artifact.VerifyDigest(digest, data) != nil {
		return nil, "", errors.New("cached referrer manifest is invalid")
	}
	return data, object.MediaType, nil
}
