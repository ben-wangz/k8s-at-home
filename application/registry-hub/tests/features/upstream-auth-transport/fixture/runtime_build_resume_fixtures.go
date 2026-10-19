package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
)

const resumeLayerSize = 4 << 20

type resumeFixture struct {
	reference      string
	mode           string
	manifest       []byte
	manifestDigest string
	layer          []byte
	layerDigest    string
	config         []byte
	configDigest   string
	fullRequests   atomic.Int32
	rangeRequests  atomic.Int32
}

type resumeBlob struct {
	fixture *resumeFixture
	data    []byte
	layer   bool
}

func (runtime *fixtureRuntime) buildResumableFixtures() error {
	runtime.resumeByReference = make(map[string]*resumeFixture)
	runtime.resumeByDigest = make(map[string]*resumeBlob)
	for _, mode := range []string{"healthy", "basic", "multi", "timeout", "idle", "restart", "slow", "concurrent", "no-range", "background", "podman", "s3", "etag-change", "range-416"} {
		fixture, err := buildResumeFixture(mode)
		if err != nil {
			return fmt.Errorf("build resumable fixture %s: %w", mode, err)
		}
		runtime.resumeByReference[fixture.reference] = fixture
		runtime.resumeByDigest[fixture.configDigest] = &resumeBlob{fixture: fixture, data: fixture.config}
		runtime.resumeByDigest[fixture.layerDigest] = &resumeBlob{fixture: fixture, data: fixture.layer, layer: true}
	}
	return nil
}

func buildResumeFixture(mode string) (*resumeFixture, error) {
	fixture := &resumeFixture{reference: "resume-" + mode, mode: mode}
	fixture.layer, fixture.layerDigest = buildResumeLayer(mode)
	diffID := fixture.layerDigest
	fixture.config, _ = json.Marshal(map[string]any{
		"architecture": "amd64", "os": "linux",
		"config":  map[string]any{"Env": []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}},
		"rootfs":  map[string]any{"type": "layers", "diff_ids": []string{diffID}},
		"history": []map[string]any{{"created_by": "registry-hub resumable-download fixture"}},
	})
	fixture.configDigest = resumeDigest(fixture.config)
	fixture.manifest, _ = json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "size": len(fixture.config), "digest": fixture.configDigest},
		"layers": []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar", "size": len(fixture.layer), "digest": fixture.layerDigest}},
	})
	fixture.manifestDigest = resumeDigest(fixture.manifest)
	return fixture, nil
}

func buildResumeLayer(mode string) ([]byte, string) {
	var data bytes.Buffer
	archive := tar.NewWriter(&data)
	pattern := []byte(mode + "-")
	content := bytes.Repeat(pattern, (resumeLayerSize+len(pattern)-1)/len(pattern))
	content = content[:resumeLayerSize]
	_ = archive.WriteHeader(&tar.Header{Name: "registry-hub-resume.txt", Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg})
	_, _ = archive.Write(content)
	_ = archive.Close()
	body := data.Bytes()
	return body, resumeDigest(body)
}

func resumeDigest(data []byte) string {
	hash := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(hash[:])
}

func (runtime *fixtureRuntime) serveResumeBlob(writer http.ResponseWriter, request *http.Request, blob *resumeBlob) {
	fixture := blob.fixture
	if !blob.layer {
		writeBlob(writer, blob.data, resumeDigest(blob.data))
		return
	}
	if request.Header.Get("Range") == "" {
		count := fixture.fullRequests.Add(1)
		if fixture.mode == "idle" && count == 1 {
			serveIdleFullBlob(writer, request, fixture, blob.data)
			return
		}
		if fixture.mode == "slow" && count == 1 {
			serveSlowFullBlob(writer, request, fixture, blob.data)
			return
		}
		if fixture.mode == "concurrent" && count == 1 {
			serveConcurrentFullBlob(writer, request, fixture, blob.data)
			return
		}
		if fixture.mode == "healthy" && count == 1 {
			serveCompleteBlob(writer, fixture, blob.data, true)
			return
		}
		if count == 1 {
			serveTruncatedFullBlob(writer, fixture, blob.data, 1<<20)
			return
		}
		serveCompleteBlob(writer, fixture, blob.data, fixture.mode != "etag-change")
		return
	}
	count := fixture.rangeRequests.Add(1)
	start, _, ok := parseFixtureRange(request.Header.Get("Range"))
	if !ok {
		writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	log.Printf("resume_fixture reference=%s mode=%s range=%q request=%d", fixture.reference, fixture.mode, request.Header.Get("Range"), count)
	if (fixture.mode == "background" || fixture.mode == "s3") && count <= 3 {
		writer.Header().Set("Retry-After", "0")
		writer.WriteHeader(http.StatusServiceUnavailable)
		log.Printf("resume_fixture reference=%s mode=%s sent=0 status=503", fixture.reference, fixture.mode)
		return
	}
	if fixture.mode == "range-416" && count == 1 {
		writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		log.Printf("resume_fixture reference=%s mode=%s sent=0 status=416", fixture.reference, fixture.mode)
		return
	}
	if fixture.mode == "restart" && count == 1 {
		serveBlockedRange(writer, request, fixture, blob.data, start)
		return
	}
	if fixture.mode == "timeout" && count == 1 {
		serveTimedOutRange(writer, request, fixture, blob.data, start)
		return
	}
	if fixture.mode == "multi" && count <= 3 {
		serveTruncatedRange(writer, fixture, blob.data, start, 1<<20, 256<<10)
		return
	}
	if fixture.mode == "etag-change" && count == 1 {
		serveRange(writer, fixture, blob.data, start, "\"resume-changed\"", len(blob.data)-1)
		return
	}
	if fixture.mode == "no-range" {
		serveCompleteBlob(writer, fixture, blob.data, true)
		return
	}
	serveRange(writer, fixture, blob.data, start, resumeETag(fixture), len(blob.data)-1)
}
