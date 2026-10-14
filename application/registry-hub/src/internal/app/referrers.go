package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/registryapi"
	"registry-hub/internal/route"
	"sort"
	"strconv"
	"strings"
	"time"
)

const referrerIndexMediaType = "application/vnd.oci.image.index.v1+json"

type referrerIndex struct {
	manifests      []json.RawMessage
	digest         string
	filtersApplied bool
}

type referrerDocument struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Manifests     []json.RawMessage `json:"manifests"`
}

type referrerDescriptor struct {
	Digest       string `json:"digest"`
	ArtifactType string `json:"artifactType"`
}

type nativeReferrerCapability struct {
	available bool
	expiresAt time.Time
}

func (s *Service) referrerSourceQuery(query url.Values) url.Values {
	result := make(url.Values)
	if artifactType := query.Get("artifactType"); artifactType != "" {
		result.Set("artifactType", artifactType)
	}
	result.Set("n", strconv.Itoa(s.cfg.Limits.MaxPageSize))
	return result
}

func (s *Service) serveLocalReferrers(writer http.ResponseWriter, request *http.Request, routed route.Route, query url.Values, response *http.Response, meta *requestMeta) {
	migrationPending := false
	confirmedLocalMiss := nativeReferrerMiss(response)
	var nativeIndex referrerIndex
	if response.StatusCode == http.StatusOK {
		var err error
		nativeIndex, err = s.collectLocalReferrers(request.Context(), routed, response, query.Get("artifactType"))
		if err != nil {
			s.metrics.recordDistributionValidation("referrers")
			meta.cacheSource = "error"
			registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "internal referrers response is invalid")
			return
		}
	}
	if response.StatusCode == http.StatusOK || confirmedLocalMiss {
		var err error
		migrationPending, err = s.referrerMigrationPending(request.Context(), routed, routed.Reference, response.StatusCode == http.StatusOK)
		if err != nil {
			_ = response.Body.Close()
			meta.cacheSource = "error"
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "referrer migration state is unavailable")
			return
		}
	}
	if response.StatusCode == http.StatusOK && !migrationPending {
		s.metrics.recordReferrerSource("native")
		meta.cacheSource = "local"
		s.writeReferrerPage(writer, request, routed, query, nativeIndex, false)
		return
	}
	if response.StatusCode != http.StatusOK && !confirmedLocalMiss {
		meta.cacheSource = "local"
		s.writeRegistryResponse(writer, request, routed, response, false)
		return
	}
	_ = response.Body.Close()
	fallbackPath := "/v2/" + routed.Internal + "/manifests/" + url.PathEscape(referrerFallbackTag(routed.Reference))
	fallback, err := s.registry.Do(request.Context(), http.MethodGet, fallbackPath, nil, http.Header{"Accept": []string{manifestAccept}}, nil, 0)
	if err != nil {
		meta.cacheSource = "error"
		registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "internal fallback referrers index is unavailable")
		return
	}
	if fallback.StatusCode == http.StatusOK {
		index, readErr := s.readReferrerIndex(fallback)
		if readErr != nil {
			s.metrics.recordDistributionValidation("referrers")
			meta.cacheSource = "error"
			registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "internal fallback referrers index is invalid")
			return
		}
		if markerErr := s.recordFallbackMarker(request.Context(), routed, routed.Reference, referrerFallbackTag(routed.Reference), index.digest); markerErr != nil {
			meta.cacheSource = "error"
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "fallback referrer protection state is unavailable")
			return
		}
		s.metrics.recordReferrerSource("fallback")
		meta.cacheSource = "local"
		s.writeReferrerPage(writer, request, routed, query, index, false)
		return
	}
	if fallback.StatusCode != http.StatusNotFound || !confirmedMiss(fallback, http.MethodGet, route.EndpointManifest) {
		meta.cacheSource = "error"
		if fallback.StatusCode == http.StatusUnauthorized || fallback.StatusCode == http.StatusForbidden {
			_ = fallback.Body.Close()
			registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "internal Registry authentication failed")
			return
		}
		s.writeRegistryResponse(writer, request, routed, fallback, false)
		return
	}
	_ = fallback.Body.Close()
	if migrationPending || !routed.HasUpstream {
		meta.cacheSource = "local"
		s.writeReferrerPage(writer, request, routed, query, referrerIndex{manifests: []json.RawMessage{}}, false)
		return
	}
	s.serveUpstreamReferrers(writer, request, routed, query, meta)
}

func nativeReferrerMiss(response *http.Response) bool {
	return response != nil && response.StatusCode == http.StatusNotFound
}

func (s *Service) collectLocalReferrers(ctx context.Context, routed route.Route, first *http.Response, artifactType string) (referrerIndex, error) {
	path := registryPath(routed)
	return s.collectReferrerPages(first, path, artifactType, func(query url.Values) (*http.Response, error) {
		return s.registry.Do(ctx, http.MethodGet, path, query, http.Header{"Accept": []string{referrerIndexMediaType}}, nil, 0)
	})
}

func (s *Service) collectUpstreamReferrers(ctx context.Context, routed route.Route, first *http.Response, artifactType string) (referrerIndex, error) {
	path := upstreamPath(routed)
	return s.collectReferrerPages(first, path, artifactType, func(query url.Values) (*http.Response, error) {
		return s.fetchUpstream(ctx, routed.Upstream, routed.RemoteRepo, path, query, http.Header{"Accept": []string{referrerIndexMediaType}})
	})
}

func (s *Service) collectReferrerPages(first *http.Response, expectedPath, artifactType string, fetch func(url.Values) (*http.Response, error)) (referrerIndex, error) {
	result := referrerIndex{manifests: make([]json.RawMessage, 0)}
	seenDigests := make(map[string]json.RawMessage)
	seenPages := make(map[string]struct{})
	response := first
	for page := 0; page < s.cfg.Limits.MaxReferrerPages; page++ {
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			return referrerIndex{}, fmt.Errorf("referrers page returned status %d", response.StatusCode)
		}
		link := response.Header.Values("Link")
		if strings.Contains(strings.ToLower(response.Header.Get("OCI-Filters-Applied")), "artifacttype") {
			result.filtersApplied = true
		}
		index, err := s.readReferrerIndex(response)
		if err != nil {
			return referrerIndex{}, err
		}
		for _, descriptor := range index.manifests {
			var identity referrerDescriptor
			if json.Unmarshal(descriptor, &identity) != nil || !route.ValidDigest(identity.Digest) {
				return referrerIndex{}, errors.New("referrer descriptor identity is invalid")
			}
			compact := new(bytes.Buffer)
			if err := json.Compact(compact, descriptor); err != nil {
				return referrerIndex{}, errors.New("referrer descriptor JSON is invalid")
			}
			if existing, found := seenDigests[identity.Digest]; found {
				if !bytes.Equal(existing, compact.Bytes()) {
					return referrerIndex{}, errors.New("duplicate referrer digest has conflicting descriptors")
				}
				continue
			}
			seenDigests[identity.Digest] = append(json.RawMessage(nil), compact.Bytes()...)
			result.manifests = append(result.manifests, compact.Bytes())
			if len(result.manifests) > s.cfg.Limits.MaxDescriptors {
				return referrerIndex{}, errors.New("referrer count exceeds configured limit")
			}
		}
		next, found, err := nextReferrerPage(link, expectedPath)
		if err != nil {
			return referrerIndex{}, err
		}
		if !found {
			break
		}
		if artifactType != "" && next.Get("artifactType") == "" {
			next.Set("artifactType", artifactType)
		}
		pageKey := next.Encode()
		if _, duplicate := seenPages[pageKey]; duplicate {
			return referrerIndex{}, errors.New("referrers pagination loop detected")
		}
		seenPages[pageKey] = struct{}{}
		response, err = fetch(next)
		if err != nil {
			return referrerIndex{}, err
		}
		if page == s.cfg.Limits.MaxReferrerPages-1 {
			_ = response.Body.Close()
			return referrerIndex{}, errors.New("referrers page limit exceeded")
		}
	}
	sort.Slice(result.manifests, func(i, j int) bool {
		var left, right referrerDescriptor
		_ = json.Unmarshal(result.manifests[i], &left)
		_ = json.Unmarshal(result.manifests[j], &right)
		return left.Digest < right.Digest
	})
	canonical, err := json.Marshal(referrerDocument{SchemaVersion: 2, MediaType: referrerIndexMediaType, Manifests: result.manifests})
	if err != nil {
		return referrerIndex{}, err
	}
	result.digest = artifact.CalculateSHA256(canonical)
	return result, nil
}
