package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func serveTimedOutRange(writer http.ResponseWriter, request *http.Request, fixture *resumeFixture, data []byte, start int) {
	length := len(data) - start
	if length > 1<<20 {
		length = 1 << 20
	}
	serveRangePrefixHeaders(writer, fixture, data, start, length)
	_, _ = writer.Write(data[start : start+(256<<10)])
	flushResponse(writer)
	log.Printf("resume_fixture reference=%s mode=%s sent=%d complete=false status=206", fixture.reference, fixture.mode, 256<<10)
	select {
	case <-request.Context().Done():
	case <-time.After(4 * time.Second):
	}
}

func serveBlockedRange(writer http.ResponseWriter, request *http.Request, fixture *resumeFixture, data []byte, start int) {
	length := len(data) - start
	serveRangePrefixHeaders(writer, fixture, data, start, length)
	flushResponse(writer)
	log.Printf("resume_fixture reference=%s mode=%s blocked=true", fixture.reference, fixture.mode)
	<-request.Context().Done()
}

func serveRangePrefixHeaders(writer http.ResponseWriter, fixture *resumeFixture, data []byte, start, length int) {
	end := start + length - 1
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Content-Length", strconv.Itoa(length))
	writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	writer.Header().Set("Docker-Content-Digest", fixture.layerDigest)
	writer.Header().Set("ETag", resumeETag(fixture))
	writer.WriteHeader(http.StatusPartialContent)
}

func serveSlowFullBlob(writer http.ResponseWriter, request *http.Request, fixture *resumeFixture, data []byte) {
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
	writer.Header().Set("Docker-Content-Digest", fixture.layerDigest)
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(data[:1])
	flushResponse(writer)
	log.Printf("resume_fixture reference=%s mode=%s sent=1 complete=false stage=pause", fixture.reference, fixture.mode)
	select {
	case <-request.Context().Done():
		return
	case <-time.After(2 * time.Second):
	}
	_, _ = io.Copy(writer, bytes.NewReader(data[1:]))
	log.Printf("resume_fixture reference=%s mode=%s sent=%d complete=true status=200", fixture.reference, fixture.mode, len(data)-1)
}

func serveIdleFullBlob(writer http.ResponseWriter, request *http.Request, fixture *resumeFixture, data []byte) {
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
	writer.Header().Set("Docker-Content-Digest", fixture.layerDigest)
	writer.Header().Set("ETag", resumeETag(fixture))
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(data[:1<<20])
	flushResponse(writer)
	log.Printf("resume_fixture reference=%s mode=%s sent=%d complete=false stage=idle", fixture.reference, fixture.mode, 1<<20)
	<-request.Context().Done()
	log.Printf("resume_fixture reference=%s mode=%s cancelled=true", fixture.reference, fixture.mode)
}

func serveConcurrentFullBlob(writer http.ResponseWriter, request *http.Request, fixture *resumeFixture, data []byte) {
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
	writer.Header().Set("Docker-Content-Digest", fixture.layerDigest)
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(data[:1<<20])
	flushResponse(writer)
	log.Printf("resume_fixture reference=%s mode=%s sent=%d complete=false stage=pause", fixture.reference, fixture.mode, 1<<20)
	select {
	case <-request.Context().Done():
		return
	case <-time.After(3 * time.Second):
	}
	_, _ = io.Copy(writer, bytes.NewReader(data[1<<20:]))
	log.Printf("resume_fixture reference=%s mode=%s sent=%d complete=true status=200", fixture.reference, fixture.mode, len(data)-(1<<20))
}

func parseFixtureRange(value string) (int, int, bool) {
	if !strings.HasPrefix(value, "bytes=") {
		return 0, 0, false
	}
	startText, endText, found := strings.Cut(strings.TrimPrefix(value, "bytes="), "-")
	if !found {
		return 0, 0, false
	}
	start, err := strconv.Atoi(startText)
	if err != nil {
		return 0, 0, false
	}
	end, err := strconv.Atoi(endText)
	return start, end, err == nil && start >= 0 && end >= start
}

func resumeETag(fixture *resumeFixture) string {
	return `"` + fixture.layerDigest + `"`
}
