package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/registryapi"
	"registry-hub/internal/route"
	"registry-hub/internal/upstream"
	"strconv"
	"strings"
)

func validateQuery(method string, routed route.Route, query url.Values, maxPageSize int) error {
	allowed := map[string]bool{}
	switch routed.Endpoint {
	case route.EndpointManifest:
		if method == http.MethodPut {
			allowed["tag"] = true
		}
	case route.EndpointUpload:
		if routed.UploadID == "" {
			allowed["mount"], allowed["from"], allowed["digest"], allowed["digest-algorithm"] = true, true, true, true
		} else {
			allowed["_state"] = true
			if method == http.MethodPut {
				allowed["digest"] = true
			}
		}
	case route.EndpointTags, route.EndpointCatalog:
		allowed["n"], allowed["last"] = true, true
		if routed.Endpoint == route.EndpointTags {
			allowed["_origin"] = true
		}
	case route.EndpointReferrers:
		allowed["artifactType"], allowed["n"], allowed["last"] = true, true, true
	}
	for key := range query {
		if !allowed[key] {
			return errors.New("query parameter is not allowed")
		}
	}
	for _, key := range []string{"tag", "mount", "from", "digest-algorithm", "_state", "artifactType"} {
		if len(query[key]) > 1 && key != "tag" && key != "digest" {
			return errors.New("query parameter is repeated")
		}
	}
	if routed.Endpoint == route.EndpointUpload {
		if routed.UploadID == "" {
			if method != http.MethodPost || len(query["digest"]) > 1 {
				return errors.New("upload creation query is invalid")
			}
		} else {
			if method == http.MethodPut && len(query["digest"]) == 0 {
				return errors.New("upload completion digest is required")
			}
			if method != http.MethodPut && len(query["digest"]) > 0 {
				return errors.New("upload digest is only valid during completion")
			}
		}
		if len(query["digest-algorithm"]) > 0 && len(query["digest"]) > 0 && routed.UploadID != "" {
			return errors.New("digest-algorithm is only valid when creating a chunked upload")
		}
	}
	for _, key := range []string{"n", "last"} {
		if len(query[key]) > 1 || len(query[key]) == 1 && query.Get(key) == "" {
			return errors.New("pagination parameter is invalid")
		}
	}
	if len(query["_origin"]) > 1 || len(query["_origin"]) == 1 && (routed.Endpoint != route.EndpointTags || query.Get("_origin") != "upstream" || !routed.HasUpstream) {
		return errors.New("pagination origin is invalid")
	}
	if len(query["tag"]) > 32 {
		return errors.New("too many tag query parameters")
	}
	if artifactType := query.Get("artifactType"); len(artifactType) > 512 || strings.ContainsAny(artifactType, "\x00\r\n") {
		return errors.New("artifactType filter is invalid")
	}
	for _, tag := range query["tag"] {
		if !route.ValidReference(tag) || strings.Contains(tag, ":") {
			return errors.New("tag query is invalid")
		}
	}
	if len(query["mount"]) > 0 && !route.ValidDigest(query.Get("mount")) {
		return errors.New("mount digest is invalid")
	}
	if len(query["from"]) > 0 {
		if err := route.ValidateRepository(query.Get("from")); err != nil {
			return err
		}
	}
	if len(query["digest"]) > 0 {
		for _, digest := range query["digest"] {
			if !route.ValidDigest(digest) {
				return errors.New("digest query is invalid")
			}
		}
	}
	if len(query["digest-algorithm"]) > 0 && !route.ValidDigestAlgorithm(query.Get("digest-algorithm")) {
		return errors.New("digest algorithm query is invalid")
	}
	if value := query.Get("n"); value != "" {
		number, err := strconv.Atoi(value)
		if err != nil || number < 0 || number > maxPageSize {
			return errors.New("page size is invalid")
		}
	}
	if value := query.Get("last"); value != "" {
		if routed.Endpoint == route.EndpointTags {
			if !route.ValidReference(value) || strings.Contains(value, ":") {
				return errors.New("last tag is invalid")
			}
		} else if routed.Endpoint == route.EndpointCatalog {
			if route.ValidateRepository(value) != nil {
				return errors.New("last repository is invalid")
			}
		}
	}
	return nil
}

func mapUploadQuery(query url.Values) (url.Values, error) {
	internal := cloneValues(query)
	if from := query.Get("from"); from != "" {
		name, err := route.InternalName(from)
		if err != nil {
			return nil, err
		}
		internal.Set("from", name)
	}
	return internal, nil
}

func retryableStatus(status int) bool {
	return upstream.RetryableStatus(status)
}

func confirmedMiss(response *http.Response, method string, endpoint route.Endpoint) bool {
	if response.StatusCode != http.StatusNotFound {
		return false
	}
	if method == http.MethodHead {
		return true
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	_ = response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(data))
	if err != nil {
		return false
	}
	var envelope registryapi.ErrorEnvelope
	if json.Unmarshal(data, &envelope) != nil || len(envelope.Errors) == 0 {
		return false
	}
	for _, item := range envelope.Errors {
		switch endpoint {
		case route.EndpointManifest:
			if item.Code == "MANIFEST_UNKNOWN" || item.Code == "NAME_UNKNOWN" {
				return true
			}
		case route.EndpointBlob:
			if item.Code == "BLOB_UNKNOWN" || item.Code == "NAME_UNKNOWN" {
				return true
			}
		case route.EndpointTags, route.EndpointReferrers:
			if item.Code == "NAME_UNKNOWN" || item.Code == "MANIFEST_UNKNOWN" {
				return true
			}
		}
	}
	return false
}

func writeMiss(writer http.ResponseWriter, endpoint route.Endpoint) {
	switch endpoint {
	case route.EndpointBlob:
		registryapi.WriteError(writer, http.StatusNotFound, "BLOB_UNKNOWN", "blob is unknown")
	case route.EndpointManifest:
		registryapi.WriteError(writer, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest is unknown")
	default:
		registryapi.WriteError(writer, http.StatusNotFound, "NAME_UNKNOWN", "repository is unknown")
	}
}

func readUpstreamManifest(response *http.Response, requestedReference string, limit int64) ([]byte, string, string, error) {
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, "", "", errors.New("upstream manifest body is invalid or too large")
	}
	if response.ContentLength >= 0 && response.ContentLength != int64(len(data)) {
		return nil, "", "", errors.New("upstream manifest length does not match Content-Length")
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType == "" {
		return nil, "", "", errors.New("upstream manifest Content-Type is invalid")
	}
	digest := response.Header.Get("Docker-Content-Digest")
	if strings.Contains(requestedReference, ":") {
		if digest != "" && digest != requestedReference {
			return nil, "", "", fmt.Errorf("%w: upstream manifest digest header does not match request", artifact.ErrDigestMismatch)
		}
		digest = requestedReference
	} else if digest == "" {
		digest = artifact.CalculateSHA256(data)
	}
	if err := artifact.VerifyDigest(digest, data); err != nil {
		return nil, "", "", err
	}
	return data, mediaType, digest, nil
}

func upstreamFailureClass(err error) string {
	switch {
	case errors.Is(err, artifact.ErrDigestMismatch):
		return "digest_mismatch"
	case errors.Is(err, upstream.ErrSSRFRejected):
		return "ssrf"
	case errors.Is(err, upstream.ErrSecurityPolicyRejected):
		return "upstream_policy"
	default:
		return "upstream"
	}
}

func manifestValidationFailureClass(err error) string {
	if errors.Is(err, artifact.ErrDigestMismatch) {
		return "digest_mismatch"
	}
	return "manifest_invalid"
}
