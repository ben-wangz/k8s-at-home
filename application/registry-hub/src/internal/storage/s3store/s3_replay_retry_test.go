package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/storage"
	"sync"
	"testing"
)

func TestPutObjectRetriesWithAnIdenticalReplayableBody(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	var operationIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPut {
			http.NotFound(writer, request)
			return
		}
		var body []byte
		var err error
		mu.Lock()
		attempt := len(bodies) + 1
		mu.Unlock()
		if attempt == 1 {
			body, err = io.ReadAll(io.LimitReader(request.Body, 7))
		} else {
			body, err = io.ReadAll(request.Body)
		}
		if err != nil {
			http.Error(writer, "read failed", http.StatusBadRequest)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		operationIDs = append(operationIDs, request.Header.Get("x-amz-meta-registry-hub-operation"))
		attempt = len(bodies)
		mu.Unlock()
		if attempt == 1 {
			writer.Header().Set("Content-Type", "application/xml")
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(writer, `<Error><Code>InternalError</Code></Error>`)
			return
		}
		writer.Header().Set("ETag", `"put-etag"`)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	store := newReplayRetryTestStore(t, server.URL, 1<<20, 5<<20)
	defer store.Close()

	data := []byte("retryable S3 PutObject payload")
	input := &replayCountingReader{reader: bytes.NewReader(data)}
	entry, err := store.Put(context.Background(), "test/replay-single", input, int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err != nil {
		t.Fatalf("Put after SDK retry: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if entry.Version != `"put-etag"` || len(bodies) != 2 {
		t.Fatalf("Put entry=%#v attempts=%d", entry, len(bodies))
	}
	if !bytes.Equal(bodies[0], data[:7]) || !bytes.Equal(bodies[1], data) {
		t.Fatalf("Put retry bodies = %q, %q; want a partial first request and the complete replay", bodies[0], bodies[1])
	}
	if input.bytesRead != len(data) {
		t.Fatalf("source bytes read = %d, want one pass over %d bytes", input.bytesRead, len(data))
	}
	if operationIDs[0] == "" || operationIDs[0] != operationIDs[1] {
		t.Fatalf("Put operation IDs = %q, %q", operationIDs[0], operationIDs[1])
	}
}

func TestMultipartPartRetryReusesUploadIDPartNumberAndBytes(t *testing.T) {
	var mu sync.Mutex
	var uploadIDs []string
	var partNumbers []string
	partBodies := map[string][][]byte{}
	acceptedParts := map[string][]byte{}
	var completedObject []byte
	var operationID string
	var completeCount int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Query().Has("uploads"):
			mu.Lock()
			operationID = request.Header.Get("x-amz-meta-registry-hub-operation")
			mu.Unlock()
			writer.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(writer, `<InitiateMultipartUploadResult><UploadId>retry-upload</UploadId></InitiateMultipartUploadResult>`)
		case request.Method == http.MethodPut && request.URL.Query().Get("uploadId") == "retry-upload":
			partNumber := request.URL.Query().Get("partNumber")
			body, err := io.ReadAll(request.Body)
			if err != nil {
				http.Error(writer, "read failed", http.StatusBadRequest)
				return
			}
			mu.Lock()
			partBodies[partNumber] = append(partBodies[partNumber], body)
			uploadIDs = append(uploadIDs, request.URL.Query().Get("uploadId"))
			partNumbers = append(partNumbers, partNumber)
			attempt := len(partBodies[partNumber])
			mu.Unlock()
			if partNumber == "1" && attempt == 1 {
				writer.Header().Set("Content-Type", "application/xml")
				writer.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(writer, `<Error><Code>InternalError</Code></Error>`)
				return
			}
			mu.Lock()
			acceptedParts[partNumber] = append([]byte(nil), body...)
			mu.Unlock()
			writer.Header().Set("ETag", `"part-`+partNumber+`-etag"`)
		case request.Method == http.MethodPost && request.URL.Query().Get("uploadId") == "retry-upload":
			mu.Lock()
			completeCount++
			completedObject = append(completedObject, acceptedParts["1"]...)
			completedObject = append(completedObject, acceptedParts["2"]...)
			mu.Unlock()
			writer.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(writer, `<CompleteMultipartUploadResult><ETag>"complete-etag"</ETag></CompleteMultipartUploadResult>`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	store := newReplayRetryTestStore(t, server.URL, 1, 5<<20)
	defer store.Close()

	data := append(bytes.Repeat([]byte("x"), 5<<20), []byte("final-part")...)
	entry, err := store.Put(context.Background(), "test/replay-multipart", bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err != nil {
		t.Fatalf("multipart Put after SDK retry: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if entry.Version != `"complete-etag"` || entry.Size != int64(len(data)) || len(partBodies["1"]) != 2 || len(partBodies["2"]) != 1 {
		t.Fatalf("multipart entry=%#v part attempts=%v", entry, partBodies)
	}
	firstPart := data[:5<<20]
	secondPart := data[5<<20:]
	if !bytes.Equal(partBodies["1"][0], firstPart) || !bytes.Equal(partBodies["1"][1], firstPart) || !bytes.Equal(partBodies["2"][0], secondPart) {
		t.Fatalf("multipart part bodies do not match the expected source ranges")
	}
	if operationID == "" || len(uploadIDs) != 3 || uploadIDs[0] != "retry-upload" || uploadIDs[1] != uploadIDs[0] || uploadIDs[2] != uploadIDs[0] ||
		len(partNumbers) != 3 || partNumbers[0] != "1" || partNumbers[1] != "1" || partNumbers[2] != "2" || completeCount != 1 {
		t.Fatalf("multipart identity operation=%q upload IDs=%v part numbers=%v completes=%d", operationID, uploadIDs, partNumbers, completeCount)
	}
	if !bytes.Equal(completedObject, data) {
		t.Fatalf("completed multipart bytes differ from source: got %d bytes, want %d", len(completedObject), len(data))
	}
	if sha256.Sum256(completedObject) != sha256.Sum256(data) {
		t.Fatal("completed multipart digest differs from source")
	}
}
