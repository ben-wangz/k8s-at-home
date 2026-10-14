package app

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
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

func (s *Service) readReferrerIndex(response *http.Response) (referrerIndex, error) {
	defer response.Body.Close()
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != referrerIndexMediaType {
		return referrerIndex{}, invalidReferrer("referrers response is not an OCI image index")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, s.cfg.Limits.ManifestBytes+1))
	if err != nil {
		return referrerIndex{}, err
	}
	if int64(len(data)) > s.cfg.Limits.ManifestBytes {
		return referrerIndex{}, invalidReferrer("referrers response body exceeds size limit")
	}
	manifest, err := artifact.ParseManifest(data, mediaType, s.cfg.Limits.MaxDescriptors, s.cfg.Limits.DescriptorDataBytes)
	if err != nil || manifest.Manifests == nil {
		return referrerIndex{}, invalidReferrer("referrers index failed manifest validation")
	}
	var document referrerDocument
	if err := json.Unmarshal(data, &document); err != nil || document.SchemaVersion != 2 || document.MediaType != referrerIndexMediaType {
		return referrerIndex{}, invalidReferrer("referrers index fields are invalid")
	}
	descriptors, err := referrerDescriptorMap(document.Manifests)
	if err != nil {
		return referrerIndex{}, err
	}
	document.Manifests = sortedReferrerDescriptors(descriptors)
	digest := response.Header.Get("Docker-Content-Digest")
	if digest == "" {
		digest = artifact.CalculateSHA256(data)
	}
	if err := artifact.VerifyDigest(digest, data); err != nil {
		return referrerIndex{}, invalidReferrer("referrers index digest is invalid")
	}
	return referrerIndex{manifests: document.Manifests, digest: digest, filtersApplied: strings.Contains(strings.ToLower(response.Header.Get("OCI-Filters-Applied")), "artifacttype")}, nil
}

func nextReferrerPage(headers []string, expectedPath string) (url.Values, bool, error) {
	for _, header := range headers {
		for _, part := range strings.Split(header, ",") {
			start, end := strings.IndexByte(part, '<'), strings.IndexByte(part, '>')
			if start < 0 || end <= start || !strings.Contains(strings.ToLower(part[end+1:]), `rel="next"`) && !strings.Contains(strings.ToLower(part[end+1:]), "rel=next") {
				continue
			}
			target, err := url.Parse(strings.TrimSpace(part[start+1 : end]))
			if err != nil || target.User != nil || target.Fragment != "" || target.Path != "" && target.Path != expectedPath {
				return nil, false, errors.New("referrers Link target is invalid")
			}
			query := target.Query()
			if len(query) == 0 {
				return nil, false, errors.New("referrers Link has no cursor")
			}
			for key, values := range query {
				if key != "artifactType" && key != "n" && key != "last" || len(values) != 1 {
					return nil, false, errors.New("referrers Link query is invalid")
				}
			}
			return query, true, nil
		}
	}
	return nil, false, nil
}

func (s *Service) writeReferrerPage(writer http.ResponseWriter, request *http.Request, routed route.Route, query url.Values, index referrerIndex, sourceFiltered bool) {
	manifests := append([]json.RawMessage(nil), index.manifests...)
	if manifests == nil {
		manifests = []json.RawMessage{}
	}
	sort.Slice(manifests, func(i, j int) bool {
		var left, right referrerDescriptor
		_ = json.Unmarshal(manifests[i], &left)
		_ = json.Unmarshal(manifests[j], &right)
		return left.Digest < right.Digest
	})
	indexDigest := index.digest
	if indexDigest == "" {
		canonical, _ := json.Marshal(referrerDocument{SchemaVersion: 2, MediaType: referrerIndexMediaType, Manifests: manifests})
		indexDigest = artifact.CalculateSHA256(canonical)
	}
	artifactType := query.Get("artifactType")
	filtered := make([]json.RawMessage, 0, len(manifests))
	for _, raw := range manifests {
		var descriptor referrerDescriptor
		if json.Unmarshal(raw, &descriptor) != nil {
			registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "referrer descriptor is invalid")
			return
		}
		if sourceFiltered || index.filtersApplied || artifactType == "" || descriptor.ArtifactType == artifactType {
			filtered = append(filtered, raw)
		}
	}
	if artifactType != "" {
		writer.Header().Set("OCI-Filters-Applied", "artifactType")
	}
	if cursorValue := query.Get("last"); cursorValue != "" {
		cursor, err := verifyReferrerCursor(s.cursorKey, cursorValue, routed.Repository, routed.Reference, artifactType)
		if err != nil || cursor.IndexDigest != indexDigest {
			registryapi.WriteError(writer, http.StatusConflict, "CONFLICT", "referrers cursor is invalid or stale")
			return
		}
		position := -1
		for index, raw := range filtered {
			var descriptor referrerDescriptor
			_ = json.Unmarshal(raw, &descriptor)
			if descriptor.Digest == cursor.LastDigest {
				position = index
				break
			}
		}
		if position < 0 {
			registryapi.WriteError(writer, http.StatusConflict, "CONFLICT", "referrers cursor no longer identifies an item")
			return
		}
		filtered = filtered[position+1:]
	}
	pageSize := s.cfg.Limits.MaxPageSize
	if value := query.Get("n"); value != "" {
		pageSize, _ = strconv.Atoi(value)
	}
	more := false
	if pageSize == 0 {
		filtered = []json.RawMessage{}
	} else if len(filtered) > pageSize {
		more = true
		filtered = filtered[:pageSize]
	}
	document := referrerDocument{SchemaVersion: 2, MediaType: referrerIndexMediaType, Manifests: filtered}
	data, err := json.Marshal(document)
	if err != nil {
		registryapi.WriteError(writer, http.StatusInternalServerError, "UNKNOWN", "referrers response could not be encoded")
		return
	}
	writer.Header().Set("Content-Type", referrerIndexMediaType)
	writer.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if more && len(filtered) > 0 {
		if err := s.setReferrerCursor(writer.Header(), routed, artifactType, pageSize, indexDigest, filtered[len(filtered)-1]); err != nil {
			registryapi.WriteError(writer, http.StatusInternalServerError, "UNKNOWN", "referrers cursor could not be created")
			return
		}
	}
	writer.WriteHeader(http.StatusOK)
	if request.Method != http.MethodHead {
		_, _ = writer.Write(data)
	}
}

func (s *Service) setReferrerCursor(headers http.Header, routed route.Route, artifactType string, pageSize int, indexDigest string, last json.RawMessage) error {
	var descriptor referrerDescriptor
	if err := json.Unmarshal(last, &descriptor); err != nil || descriptor.Digest == "" {
		return errors.New("last referrer descriptor is invalid")
	}
	cursor, err := signReferrerCursor(s.cursorKey, referrerCursor{
		Repository: routed.Repository, Subject: routed.Reference, ArtifactType: artifactType,
		IndexDigest: indexDigest, LastDigest: descriptor.Digest, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		return err
	}
	query := url.Values{"n": {strconv.Itoa(pageSize)}}
	if artifactType != "" {
		query.Set("artifactType", artifactType)
	}
	query.Set("last", cursor)
	path := "/v2/" + routed.Repository + "/referrers/" + url.PathEscape(routed.Reference)
	headers.Set("Link", "<"+path+"?"+query.Encode()+`>; rel="next"`)
	return nil
}

func referrerFallbackTag(digest string) string {
	algorithm, encoded, found := strings.Cut(digest, ":")
	if !found {
		return ""
	}
	if len(algorithm) > 32 {
		algorithm = algorithm[:32]
	}
	if len(encoded) > 64 {
		encoded = encoded[:64]
	}
	tag := algorithm + "-" + encoded
	for index, character := range tag {
		valid := character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' || character == '.' || character == '-'
		if !valid || index == 0 && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			tag = tag[:index] + "-" + tag[index+1:]
		}
	}
	return tag
}

func upstreamManifestPath(routed route.Route, tag string) string {
	return "/v2/" + routed.RemoteRepo + "/manifests/" + url.PathEscape(tag)
}
