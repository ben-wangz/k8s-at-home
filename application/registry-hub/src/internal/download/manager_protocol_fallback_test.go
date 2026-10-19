package download

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"

	"registry-hub/internal/artifact"
)

func TestFetchFallsBackToFullResponseWhenStrongETagChanges(t *testing.T) {
	ctx, store := testDownloadStore(t)
	content := []byte("abcdefgh")
	digest := artifact.CalculateSHA256(content)
	manager := testManager(store)
	var starts []int64
	call := 0
	request := testRequest(content, digest, 13, func(_ context.Context, start, _ int64, _ string) (*http.Response, context.CancelFunc, error) {
		call++
		starts = append(starts, start)
		switch call {
		case 1:
			response := testResponse(http.StatusOK, content, 8, &readThenError{reader: bytes.NewReader(content[:4])})
			response.Header.Set("Docker-Content-Digest", digest)
			return response, func() {}, nil
		case 2:
			response := testResponse(http.StatusPartialContent, content[4:], 4, io.NopCloser(bytes.NewReader(content[4:])))
			response.Header.Set("Content-Range", "bytes 4-7/8")
			response.Header.Set("ETag", "\"blob-v2\"")
			response.Header.Set("Docker-Content-Digest", digest)
			return response, func() {}, nil
		default:
			response := testResponse(http.StatusOK, content, 8, io.NopCloser(bytes.NewReader(content)))
			response.Header.Set("Docker-Content-Digest", digest)
			return response, func() {}, nil
		}
	})
	if _, err := manager.Fetch(ctx, request); err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if !reflect.DeepEqual(starts, []int64{0, 4, 0}) {
		t.Fatalf("request offsets=%v", starts)
	}
}

func TestFetchAttemptTimeoutRetriesSameCommittedOffset(t *testing.T) {
	ctx, store := testDownloadStore(t)
	content := []byte("abcdefgh")
	digest := artifact.CalculateSHA256(content)
	manager := testManager(store)
	var starts []int64
	request := testRequest(content, digest, 14, func(ctx context.Context, start, _ int64, _ string) (*http.Response, context.CancelFunc, error) {
		starts = append(starts, start)
		if start == 0 {
			response := testResponse(http.StatusOK, content, 8, &readThenError{reader: bytes.NewReader(content[:4])})
			response.Header.Set("Docker-Content-Digest", digest)
			return response, func() {}, nil
		}
		attemptCtx, cancel := context.WithCancel(ctx)
		if len(starts) == 2 {
			response := testResponse(http.StatusPartialContent, content[4:], 4, &cancelledBody{done: attemptCtx.Done()})
			response.Header.Set("Content-Range", "bytes 4-7/8")
			response.Header.Set("Docker-Content-Digest", digest)
			return response, cancel, nil
		}
		response := testResponse(http.StatusPartialContent, content[4:], 4, io.NopCloser(bytes.NewReader(content[4:])))
		response.Header.Set("Content-Range", "bytes 4-7/8")
		response.Header.Set("Docker-Content-Digest", digest)
		return response, cancel, nil
	})
	request.AttemptLimit = 25 * time.Millisecond
	if _, err := manager.Fetch(ctx, request); err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if !reflect.DeepEqual(starts, []int64{0, 4, 4}) {
		t.Fatalf("request offsets=%v", starts)
	}
}

func TestParseContentRangeRejectsOverflow(t *testing.T) {
	for _, value := range []string{"bytes 9223372036854775808-9223372036854775809/9223372036854775810", "bytes 0-1/9223372036854775808", "bytes */8"} {
		if _, _, _, err := parseContentRange(value); err == nil {
			t.Errorf("parseContentRange(%q) succeeded", value)
		}
	}
	if _, _, _, err := parseContentRange("bytes 0-7/8"); err != nil {
		t.Fatalf("valid Content-Range returned %v", err)
	}
}

type cancelledBody struct{ done <-chan struct{} }

func (b *cancelledBody) Read([]byte) (int, error) {
	<-b.done
	return 0, context.Canceled
}

func (*cancelledBody) Close() error { return nil }
