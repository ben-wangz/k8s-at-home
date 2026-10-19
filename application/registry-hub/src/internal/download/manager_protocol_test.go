package download

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"reflect"
	"testing"

	"registry-hub/internal/artifact"
)

func TestFetchContinuesShortPartialResponses(t *testing.T) {
	ctx, store := testDownloadStore(t)
	content := []byte("abcdefghij")
	digest := artifact.CalculateSHA256(content)
	manager := testManager(store)
	var starts []int64
	request := testRequest(content, digest, 10, func(_ context.Context, start, end int64, _ string) (*http.Response, context.CancelFunc, error) {
		starts = append(starts, start)
		if start == 0 {
			response := testResponse(http.StatusOK, content, int64(len(content)), &readThenError{reader: bytes.NewReader(content[:4])})
			response.Header.Set("Docker-Content-Digest", digest)
			return response, func() {}, nil
		}
		last := int64(5)
		if start == 6 {
			last = 9
		}
		if start != 4 && start != 6 || last > end {
			t.Fatalf("unexpected range start=%d end=%d", start, end)
		}
		response := testResponse(http.StatusPartialContent, content[start:last+1], last-start+1, io.NopCloser(bytes.NewReader(content[start:last+1])))
		response.Header.Set("Content-Range", "bytes "+formatOffset(start)+"-"+formatOffset(last)+"/10")
		response.Header.Set("Docker-Content-Digest", digest)
		return response, func() {}, nil
	})
	result, err := manager.Fetch(ctx, request)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if result.Size != int64(len(content)) || !reflect.DeepEqual(starts, []int64{0, 4, 6}) {
		t.Fatalf("result=%#v request offsets=%v", result, starts)
	}
	body, _, err := manager.Open(ctx, request.Identity, digest, request.MaxBytes)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	actual, readErr := io.ReadAll(body)
	_ = body.Close()
	if readErr != nil || !bytes.Equal(actual, content) {
		t.Fatalf("assembled content=%q error=%v", actual, readErr)
	}
}

func TestFetchRetriesStatusWithoutDroppingProgress(t *testing.T) {
	ctx, store := testDownloadStore(t)
	content := []byte("abcdefgh")
	digest := artifact.CalculateSHA256(content)
	manager := testManager(store)
	var starts []int64
	var retryAfter []string
	request := testRequest(content, digest, 11, func(_ context.Context, start, _ int64, _ string) (*http.Response, context.CancelFunc, error) {
		starts = append(starts, start)
		if start == 0 {
			response := testResponse(http.StatusOK, content, 8, &readThenError{reader: bytes.NewReader(content[:4])})
			response.Header.Set("Docker-Content-Digest", digest)
			return response, func() {}, nil
		}
		if len(starts) == 2 {
			response := testResponse(http.StatusServiceUnavailable, nil, 0, io.NopCloser(bytes.NewReader(nil)))
			response.Header.Set("Retry-After", "7")
			return response, func() {}, nil
		}
		response := testResponse(http.StatusPartialContent, content[4:], 4, io.NopCloser(bytes.NewReader(content[4:])))
		response.Header.Set("Content-Range", "bytes 4-7/8")
		response.Header.Set("Docker-Content-Digest", digest)
		return response, func() {}, nil
	})
	request.WaitRetry = func(_ context.Context, value string, _ int) error {
		retryAfter = append(retryAfter, value)
		return nil
	}
	if _, err := manager.Fetch(ctx, request); err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if !reflect.DeepEqual(starts, []int64{0, 4, 4}) || !reflect.DeepEqual(retryAfter, []string{"7"}) {
		t.Fatalf("request offsets=%v Retry-After values=%v", starts, retryAfter)
	}
}

func TestFetch416ResetKeepsRangeSupport(t *testing.T) {
	ctx, store := testDownloadStore(t)
	content := []byte("abcdefgh")
	digest := artifact.CalculateSHA256(content)
	manager := testManager(store)
	var starts []int64
	call := 0
	request := testRequest(content, digest, 15, func(_ context.Context, start, _ int64, _ string) (*http.Response, context.CancelFunc, error) {
		call++
		starts = append(starts, start)
		switch call {
		case 1, 3:
			response := testResponse(http.StatusOK, content, 8, &readThenError{reader: bytes.NewReader(content[:4])})
			response.Header.Set("Docker-Content-Digest", digest)
			return response, func() {}, nil
		case 2:
			response := testResponse(http.StatusRequestedRangeNotSatisfiable, nil, 0, io.NopCloser(bytes.NewReader(nil)))
			response.Header.Set("Content-Range", "bytes */8")
			return response, func() {}, nil
		default:
			response := testResponse(http.StatusPartialContent, content[4:], 4, io.NopCloser(bytes.NewReader(content[4:])))
			response.Header.Set("Content-Range", "bytes 4-7/8")
			response.Header.Set("Docker-Content-Digest", digest)
			return response, func() {}, nil
		}
	})
	if _, err := manager.Fetch(ctx, request); err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if !reflect.DeepEqual(starts, []int64{0, 4, 0, 4}) {
		t.Fatalf("request offsets=%v", starts)
	}
}

func TestFetchRejectsMalformedContinuationWithoutAdvancing(t *testing.T) {
	tests := []struct {
		name         string
		contentRange string
		encoding     string
		length       int64
	}{
		{name: "wrong start", contentRange: "bytes 5-7/8", length: 3},
		{name: "range exceeds total", contentRange: "bytes 4-8/8", length: 5},
		{name: "wrong body length", contentRange: "bytes 4-7/8", length: 3},
		{name: "encoded body", contentRange: "bytes 4-7/8", encoding: "gzip", length: 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, store := testDownloadStore(t)
			content := []byte("abcdefgh")
			digest := artifact.CalculateSHA256(content)
			manager := testManager(store)
			request := testRequest(content, digest, 12, func(_ context.Context, start, _ int64, _ string) (*http.Response, context.CancelFunc, error) {
				if start == 0 {
					response := testResponse(http.StatusOK, content, 8, &readThenError{reader: bytes.NewReader(content[:4])})
					response.Header.Set("Docker-Content-Digest", digest)
					return response, func() {}, nil
				}
				response := testResponse(http.StatusPartialContent, content[4:], test.length, io.NopCloser(bytes.NewReader(content[4:])))
				response.Header.Set("Content-Range", test.contentRange)
				response.Header.Set("Docker-Content-Digest", digest)
				if test.encoding != "" {
					response.Header.Set("Content-Encoding", test.encoding)
				}
				return response, func() {}, nil
			})
			if _, err := manager.Fetch(ctx, request); err == nil {
				t.Fatal("Fetch() accepted malformed continuation")
			}
			data, _, err := manager.readState(ctx, stateKey(request.Identity, digest))
			if err != nil {
				t.Fatalf("readState() error = %v", err)
			}
			value, err := decodeState(data, request)
			if err != nil || value.Offset != 4 {
				t.Fatalf("state offset=%d error=%v, want offset 4", value.Offset, err)
			}
		})
	}
}
