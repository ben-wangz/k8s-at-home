package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"registry-hub/internal/registryapi"
	"registry-hub/internal/route"
	"strconv"
	"strings"
)

func knownAcceptSupported(value string) bool {
	if strings.TrimSpace(value) == "" {
		return true
	}
	for _, item := range strings.Split(value, ",") {
		media, params, err := mime.ParseMediaType(strings.TrimSpace(item))
		if err != nil {
			continue
		}
		quality := 1.0
		if rawQuality := params["q"]; rawQuality != "" {
			quality, err = strconv.ParseFloat(rawQuality, 64)
		}
		if err != nil || media == "" || quality <= 0 || quality > 1 {
			continue
		}
		for _, supported := range []string{
			"application/vnd.oci.image.index.v1+json",
			"application/vnd.docker.distribution.manifest.list.v2+json",
			"application/vnd.oci.image.manifest.v1+json",
			"application/vnd.docker.distribution.manifest.v2+json",
		} {
			if acceptsMedia(item, supported) {
				return true
			}
		}
	}
	return false
}

func combinedAccept(header http.Header) string {
	return strings.Join(header.Values("Accept"), ", ")
}

func manifestAcceptFor(clientAccept string) string {
	clientAccept = strings.TrimSpace(clientAccept)
	if clientAccept == "" {
		return manifestAccept + ", */*"
	}
	return manifestAccept + ", " + clientAccept
}

func acceptsMedia(accept, mediaType string) bool {
	if accept == "" || accept == "*/*" {
		return true
	}
	actual, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		return false
	}
	for _, item := range strings.Split(accept, ",") {
		candidate, params, parseErr := mime.ParseMediaType(strings.TrimSpace(item))
		quality := 1.0
		if rawQuality := params["q"]; rawQuality != "" {
			quality, parseErr = strconv.ParseFloat(rawQuality, 64)
		}
		if parseErr != nil || quality <= 0 || quality > 1 {
			continue
		}
		if candidate == "*/*" || candidate == actual || strings.HasSuffix(candidate, "/*") && strings.HasPrefix(actual, strings.TrimSuffix(candidate, "*")) {
			return true
		}
	}
	return false
}

func writeUpstreamError(writer http.ResponseWriter, endpoint route.Endpoint, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		writer.Header().Set("Retry-After", "5")
		registryapi.WriteError(writer, http.StatusGatewayTimeout, "UNAVAILABLE", "upstream request timed out")
		return
	}
	registryapi.WriteError(writer, http.StatusBadGateway, "UNKNOWN", "upstream Registry is unavailable")
}

func writeUpstreamStatus(writer http.ResponseWriter, status int, endpoint route.Endpoint) {
	if status == http.StatusNotFound {
		writeMiss(writer, endpoint)
		return
	}
	if status == http.StatusRequestedRangeNotSatisfiable {
		registryapi.WriteError(writer, status, "RANGE_INVALID", "requested byte range is not satisfiable")
		return
	}
	code := "UNKNOWN"
	if status == http.StatusUnauthorized {
		code = "UNAUTHORIZED"
	} else if status == http.StatusForbidden {
		code = "DENIED"
	} else if status == http.StatusTooManyRequests {
		code = "TOOMANYREQUESTS"
	} else if status >= 500 {
		code = "UNAVAILABLE"
	}
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	registryapi.WriteError(writer, status, code, "upstream Registry request failed")
}

func writeResponseError(writer http.ResponseWriter, response *http.Response, endpoint route.Endpoint, isUpstream bool) {
	status := response.StatusCode
	rangeHeader := ""
	contentRange := ""
	if status == http.StatusRequestedRangeNotSatisfiable {
		rangeHeader = response.Header.Get("Range")
		contentRange = response.Header.Get("Content-Range")
	}
	clearRegistryErrorHeaders(writer.Header())
	if rangeHeader != "" {
		writer.Header().Set("Range", rangeHeader)
	}
	if contentRange != "" {
		writer.Header().Set("Content-Range", contentRange)
	}
	if status == http.StatusNotFound {
		writeMiss(writer, endpoint)
		return
	}
	if isUpstream {
		writeUpstreamStatus(writer, status, endpoint)
		return
	}
	data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	var envelope registryapi.ErrorEnvelope
	code := "UNKNOWN"
	if json.Unmarshal(data, &envelope) == nil && len(envelope.Errors) > 0 {
		code = safeErrorCode(envelope.Errors[0].Code)
	}
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	registryapi.WriteError(writer, status, code, genericErrorMessage(code))
}

func clearRegistryErrorHeaders(headers http.Header) {
	for _, name := range []string{
		"Content-Length", "Content-Encoding", "Range", "Content-Range", "Accept-Ranges",
		"Docker-Content-Digest", "Docker-Upload-UUID", "OCI-Chunk-Min-Length", "ETag", "Last-Modified",
	} {
		headers.Del(name)
	}
}

func safeErrorCode(code string) string {
	allowed := map[string]bool{
		"BLOB_UNKNOWN": true, "BLOB_UPLOAD_INVALID": true, "BLOB_UPLOAD_UNKNOWN": true, "DIGEST_INVALID": true,
		"MANIFEST_BLOB_UNKNOWN": true, "MANIFEST_INVALID": true, "MANIFEST_UNKNOWN": true, "NAME_INVALID": true,
		"NAME_UNKNOWN": true, "PAGINATION_NUMBER_INVALID": true, "RANGE_INVALID": true, "SIZE_INVALID": true,
		"MANIFEST_UNVERIFIED": true, "TAG_INVALID": true, "TOOMANYREQUESTS": true, "UNAUTHORIZED": true,
		"DENIED": true, "UNSUPPORTED": true,
	}
	if allowed[code] {
		return code
	}
	return "UNKNOWN"
}

func genericErrorMessage(code string) string {
	switch code {
	case "UNAUTHORIZED":
		return "authentication is required"
	case "DENIED":
		return "operation is denied"
	case "MANIFEST_UNKNOWN":
		return "manifest is unknown"
	case "BLOB_UNKNOWN":
		return "blob is unknown"
	case "NAME_UNKNOWN":
		return "repository is unknown"
	case "TOOMANYREQUESTS":
		return "request rate limit exceeded"
	default:
		return "Registry request failed"
	}
}

func copyRegistryHeaders(destination, source http.Header) {
	for _, name := range []string{
		"Content-Type", "Content-Length", "Content-Encoding", "Range", "Content-Range", "Accept-Ranges", "Docker-Content-Digest",
		"Docker-Upload-UUID", "OCI-Chunk-Min-Length", "OCI-Subject", "OCI-Tag", "ETag", "Last-Modified", "Retry-After",
	} {
		for _, value := range source.Values(name) {
			destination.Add(name, value)
		}
	}
}

func writeManifestBody(writer http.ResponseWriter, request *http.Request, repository string, data []byte, mediaType, digest string) {
	writer.Header().Set("Content-Type", mediaType)
	writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
	writer.Header().Set("Docker-Content-Digest", digest)
	writer.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	writer.WriteHeader(http.StatusOK)
	if request.Method != http.MethodHead {
		_, _ = writer.Write(data)
	}
}

func writeHead(writer http.ResponseWriter, mediaType, digest string, size int64) {
	writer.Header().Set("Content-Type", mediaType)
	writer.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	writer.Header().Set("Docker-Content-Digest", digest)
	writer.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	writer.WriteHeader(http.StatusOK)
}
