package main

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
)

func serveTruncatedFullBlob(writer http.ResponseWriter, fixture *resumeFixture, data []byte, length int) {
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
	writer.Header().Set("Docker-Content-Digest", fixture.layerDigest)
	writer.Header().Set("ETag", resumeETag(fixture))
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(data[:length])
	flushResponse(writer)
	log.Printf("resume_fixture reference=%s mode=%s sent=%d complete=false", fixture.reference, fixture.mode, length)
}

func serveCompleteBlob(writer http.ResponseWriter, fixture *resumeFixture, data []byte, withETag bool) {
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
	writer.Header().Set("Docker-Content-Digest", fixture.layerDigest)
	if withETag {
		writer.Header().Set("ETag", `"resume-fixture-v1"`)
	}
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(data)
	log.Printf("resume_fixture reference=%s mode=%s sent=%d complete=true status=200", fixture.reference, fixture.mode, len(data))
}

func serveRange(writer http.ResponseWriter, fixture *resumeFixture, data []byte, start int, etag string, end int) {
	if start < 0 || start >= len(data) || end < start {
		writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if end >= len(data) {
		end = len(data) - 1
	}
	body := data[start : end+1]
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	writer.Header().Set("Docker-Content-Digest", fixture.layerDigest)
	if etag != "" {
		writer.Header().Set("ETag", etag)
	}
	writer.WriteHeader(http.StatusPartialContent)
	_, _ = writer.Write(body)
	log.Printf("resume_fixture reference=%s mode=%s sent=%d complete=true status=206", fixture.reference, fixture.mode, len(body))
}

func serveTruncatedRange(writer http.ResponseWriter, fixture *resumeFixture, data []byte, start, length, send int) {
	end := start + length - 1
	if end >= len(data) {
		end = len(data) - 1
	}
	body := data[start : end+1]
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	writer.Header().Set("Docker-Content-Digest", fixture.layerDigest)
	writer.Header().Set("ETag", resumeETag(fixture))
	writer.WriteHeader(http.StatusPartialContent)
	_, _ = writer.Write(body[:send])
	flushResponse(writer)
	log.Printf("resume_fixture reference=%s mode=%s sent=%d complete=false status=206", fixture.reference, fixture.mode, send)
}

func flushResponse(writer http.ResponseWriter) {
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
}
