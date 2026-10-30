package app

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/registryapi"
	"registry-hub/internal/route"
	"strconv"
	"strings"
)

func (s *Service) serveCached(writer http.ResponseWriter, request *http.Request, routed route.Route, object artifact.CachedObject, blob bool) {
	if !blob {
		data, err := io.ReadAll(io.LimitReader(object.Body, s.cfg.Limits.ManifestBytes+1))
		if err != nil || int64(len(data)) != object.Size {
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "cached manifest is unavailable")
			return
		}
		writeManifestBody(writer, request, routed.Repository, data, object.MediaType, object.Digest)
		return
	}
	etag := `"` + object.Digest + `"`
	rangeValue := request.Header.Get("Range")
	if request.Method == http.MethodHead || request.Header.Get("If-Range") != "" && request.Header.Get("If-Range") != etag {
		rangeValue = ""
	}
	start, end, partial, err := requestedRange(rangeValue, object.Size)
	if err != nil {
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", object.Size))
		registryapi.WriteError(writer, http.StatusRequestedRangeNotSatisfiable, "RANGE_INVALID", "requested byte range is invalid")
		return
	}
	writer.Header().Set("Content-Type", object.MediaType)
	writer.Header().Set("Docker-Content-Digest", object.Digest)
	writer.Header().Set("ETag", etag)
	writer.Header().Set("Accept-Ranges", "bytes")
	writer.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	if partial {
		length := end - start + 1
		writer.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, object.Size))
		writer.WriteHeader(http.StatusPartialContent)
		if request.Method != http.MethodHead {
			_, _ = io.CopyN(io.Discard, object.Body, start)
			_, _ = io.CopyN(writer, object.Body, length)
		}
		return
	}
	writer.Header().Set("Content-Length", strconv.FormatInt(object.Size, 10))
	writer.WriteHeader(http.StatusOK)
	if request.Method != http.MethodHead {
		_, _ = io.Copy(writer, object.Body)
	}
}

func (s *Service) pushManifest(writer http.ResponseWriter, request *http.Request, routed route.Route, query url.Values, meta *requestMeta) {
	prepared, ok := s.prepareManifestPush(writer, request, routed, query)
	if !ok {
		return
	}
	authority, repository := prepared.authority, prepared.repository
	digest, mediaType, data := prepared.digest, prepared.mediaType, prepared.data
	parsedManifest, tags, inline := prepared.manifest, prepared.tags, prepared.inline
	if !s.validateUnprotectedManifestTags(writer, request, routed, tags, meta) {
		return
	}
	releasePush, ok := s.acquireSlot(request.Context(), s.pushSlots)
	if !ok {
		s.metrics.recordRejection("push_concurrency")
		writer.Header().Set("Retry-After", "1")
		registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "push concurrency limit exceeded")
		return
	}
	defer releasePush()
	releaseGate, err := s.gate.Enter(request.Context())
	if err != nil {
		if isStorageFailure(err) {
			meta.errorClass = "storage"
		}
		writer.Header().Set("Retry-After", "5")
		registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "Registry writes are blocked")
		return
	}
	defer releaseGate()
	contentCtx, releaseContent, ok := s.acquireManifestContentLease(writer, request, routed, authority, repository, digest, meta)
	if !ok {
		return
	}
	if releaseContent != nil {
		defer releaseContent()
	}
	leases, ok := s.acquireManifestTagLeases(writer, request, authority, repository, tags, meta)
	if !ok {
		return
	}
	defer releaseTagLeases(leases)
	putCtx := request.Context()
	if contentCtx != nil {
		putCtx = contentCtx
	} else if len(leases) > 0 {
		putCtx = leases[0].ctx
	}
	if !s.validateManifestTagLeases(writer, routed, authority, repository, digest, leases, meta) {
		return
	}
	embeddedReleases, err := s.publishEmbeddedObjects(putCtx, routed, inline)
	if err != nil {
		if isStorageFailure(err) {
			meta.errorClass = "storage"
		}
		writer.Header().Set("Retry-After", "5")
		registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "inline manifest objects could not be confirmed")
		return
	}
	defer releaseContentLeases(embeddedReleases)
	response, err := s.registry.Do(putCtx, http.MethodPut, "/v2/"+routed.Internal+"/manifests/"+url.PathEscape(routed.Reference), nil, http.Header{"Content-Type": []string{mediaType}}, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "internal Registry is unavailable")
		return
	}
	if response.StatusCode != http.StatusCreated {
		s.writeRegistryResponse(writer, request, routed, response, false)
		return
	}
	returnedDigest := response.Header.Get("Docker-Content-Digest")
	nativeSubjectHeader := response.Header.Get("OCI-Subject")
	_ = response.Body.Close()
	if returnedDigest != digest {
		registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "internal Registry returned a different manifest digest")
		return
	}
	defer s.inventoryDistributionChanged(request.Context(), routed.Internal)
	accepted, nativeSubjectHeader, ok := s.publishManifestTags(writer, request, routed, mediaType, data, digest, tags, leases, nativeSubjectHeader)
	if !ok {
		return
	}
	if parsedManifest.Subject != nil {
		subject, publishErr := s.publishReferrerManifest(request.Context(), routed, data, mediaType, digest, nativeSubjectHeader, true)
		if publishErr != nil {
			if isStorageFailure(publishErr) {
				meta.errorClass = "storage"
			}
			setAcceptedTags(writer, accepted)
			writer.Header().Set("Retry-After", "5")
			registryapi.WriteError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "referrer index update could not be confirmed")
			return
		}
		writer.Header().Set("OCI-Subject", subject)
	}
	meta.cacheSource = "local"
	writer.Header().Set("Docker-Content-Digest", digest)
	writer.Header().Set("Location", "/v2/"+routed.Repository+"/manifests/"+url.PathEscape(routed.Reference))
	writer.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	setAcceptedTags(writer, accepted)
	writer.WriteHeader(http.StatusCreated)
}

type preparedManifestPush struct {
	authority  string
	repository string
	digest     string
	mediaType  string
	data       []byte
	manifest   artifact.Manifest
	tags       []string
	inline     *inlineObjects
}

func (s *Service) prepareManifestPush(writer http.ResponseWriter, request *http.Request, routed route.Route, query url.Values) (preparedManifestPush, bool) {
	var prepared preparedManifestPush
	if request.ContentLength > s.cfg.Limits.ManifestBytes {
		registryapi.WriteError(writer, http.StatusRequestEntityTooLarge, "SIZE_INVALID", "manifest exceeds configured limit")
		return prepared, false
	}
	data, err := io.ReadAll(io.LimitReader(http.MaxBytesReader(writer, request.Body, s.cfg.Limits.ManifestBytes), s.cfg.Limits.ManifestBytes+1))
	if err != nil || int64(len(data)) > s.cfg.Limits.ManifestBytes {
		registryapi.WriteError(writer, http.StatusRequestEntityTooLarge, "SIZE_INVALID", "manifest exceeds configured limit")
		return prepared, false
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType == "" {
		registryapi.WriteError(writer, http.StatusBadRequest, "MANIFEST_INVALID", "manifest Content-Type is invalid")
		return prepared, false
	}
	parsedManifest, err := artifact.ParseManifest(data, mediaType, s.cfg.Limits.MaxDescriptors, s.cfg.Limits.DescriptorDataBytes)
	if err != nil {
		registryapi.WriteError(writer, http.StatusBadRequest, "MANIFEST_INVALID", "manifest failed validation")
		return prepared, false
	}
	digest := artifact.CalculateSHA256(data)
	if strings.Contains(routed.Reference, ":") {
		if err := artifact.VerifyDigest(routed.Reference, data); err != nil {
			registryapi.WriteError(writer, http.StatusBadRequest, "DIGEST_INVALID", "manifest digest does not match request")
			return prepared, false
		}
		digest = routed.Reference
	}
	tags := make([]string, 0, len(query["tag"])+1)
	seenTags := make(map[string]struct{}, len(query["tag"])+1)
	for _, tag := range query["tag"] {
		if _, seen := seenTags[tag]; seen {
			continue
		}
		seenTags[tag] = struct{}{}
		tags = append(tags, tag)
	}
	if len(tags) > 0 && !strings.Contains(routed.Reference, ":") {
		registryapi.WriteError(writer, http.StatusBadRequest, "UNSUPPORTED", "tag query is valid only with a digest reference")
		return prepared, false
	}
	if !strings.Contains(routed.Reference, ":") {
		tags = append(tags, routed.Reference)
	}
	inline, err := collectInlineObjects(digest, parsedManifest, s.cfg.Limits)
	if err != nil {
		registryapi.WriteError(writer, http.StatusBadRequest, "MANIFEST_INVALID", "manifest failed validation")
		return prepared, false
	}
	authority, repository := routeStorageIdentity(routed)
	prepared = preparedManifestPush{authority: authority, repository: repository, digest: digest, mediaType: mediaType, data: data, manifest: parsedManifest, tags: tags, inline: inline}
	return prepared, true
}
