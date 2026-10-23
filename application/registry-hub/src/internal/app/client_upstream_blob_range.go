package app

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"registry-hub/internal/registryapi"
	"registry-hub/internal/route"
)

func (s *Service) serveUpstreamBlobRange(writer http.ResponseWriter, request *http.Request, routed route.Route, response *http.Response) {
	spool, size, err := spoolResponse(response.Body, s.cfg.Limits.ObjectBytes, nil)
	if err != nil {
		registryapi.WriteError(writer, http.StatusBadGateway, "BLOB_TOO_LARGE", err.Error())
		return
	}
	defer removeSpool(spool)
	start, end, total, err := parseContentRange(response.Header.Get("Content-Range"))
	if err != nil {
		registryapi.WriteError(writer, http.StatusBadGateway, "BLOB_UNKNOWN", err.Error())
		return
	}
	if total > s.cfg.Limits.ObjectBytes {
		registryapi.WriteError(writer, http.StatusBadGateway, "BLOB_TOO_LARGE", "upstream blob exceeds the configured object limit")
		return
	}
	expectedStart, expectedEnd, _, err := requestedRange(request.Header.Get("Range"), total)
	if err != nil || start != expectedStart || end != expectedEnd || size != end-start+1 {
		registryapi.WriteError(writer, http.StatusBadGateway, "BLOB_UNKNOWN", "upstream Content-Range does not match the requested range")
		return
	}
	if response.ContentLength >= 0 && response.ContentLength != size {
		registryapi.WriteError(writer, http.StatusBadGateway, "BLOB_UNKNOWN", "upstream partial blob length does not match Content-Length")
		return
	}
	if upstreamDigest := response.Header.Get("Docker-Content-Digest"); upstreamDigest != "" && upstreamDigest != routed.Reference {
		registryapi.WriteError(writer, http.StatusBadGateway, "BLOB_UNKNOWN", "upstream blob digest header does not match the request")
		return
	}

	copyRegistryHeaders(writer.Header(), response.Header)
	writer.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
	writer.Header().Set("Docker-Content-Digest", routed.Reference)
	writer.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	writer.WriteHeader(http.StatusPartialContent)
	if _, err := spool.Seek(0, io.SeekStart); err == nil {
		_, _ = io.Copy(writer, spool)
	}
}

func spoolResponse(body io.Reader, limit int64, digestWriter io.Writer) (*os.File, int64, error) {
	spool, err := os.CreateTemp("", "registry-hub-upstream-")
	if err != nil {
		return nil, 0, err
	}
	remove := func(cause error) (*os.File, int64, error) {
		_ = spool.Close()
		_ = os.Remove(spool.Name())
		return nil, 0, cause
	}
	writer := io.Writer(spool)
	if digestWriter != nil {
		writer = io.MultiWriter(spool, digestWriter)
	}
	size, err := io.Copy(writer, io.LimitReader(body, limit+1))
	if err != nil {
		return remove(err)
	}
	if size > limit {
		return remove(fmt.Errorf("response exceeds the configured object limit of %d bytes", limit))
	}
	return spool, size, nil
}

func removeSpool(spool *os.File) {
	if spool == nil {
		return
	}
	name := spool.Name()
	_ = spool.Close()
	_ = os.Remove(name)
}

func parseContentRange(value string) (int64, int64, int64, error) {
	parts := strings.SplitN(value, " ", 2)
	if len(parts) != 2 || parts[0] != "bytes" {
		return 0, 0, 0, errors.New("invalid Content-Range")
	}
	values := strings.SplitN(parts[1], "/", 2)
	if len(values) != 2 || values[1] == "*" {
		return 0, 0, 0, errors.New("invalid Content-Range")
	}
	span := strings.SplitN(values[0], "-", 2)
	if len(span) != 2 {
		return 0, 0, 0, errors.New("invalid Content-Range")
	}
	start, err := strconv.ParseInt(span[0], 10, 64)
	if err != nil {
		return 0, 0, 0, errors.New("invalid Content-Range start")
	}
	end, err := strconv.ParseInt(span[1], 10, 64)
	if err != nil {
		return 0, 0, 0, errors.New("invalid Content-Range end")
	}
	total, err := strconv.ParseInt(values[1], 10, 64)
	if err != nil || start < 0 || end < start || total <= end {
		return 0, 0, 0, errors.New("invalid Content-Range values")
	}
	return start, end, total, nil
}
