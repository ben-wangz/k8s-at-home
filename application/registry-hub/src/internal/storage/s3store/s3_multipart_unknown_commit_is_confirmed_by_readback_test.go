package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"io"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/storage"
	"sync"
	"testing"
)

func TestMultipartUnknownCommitIsConfirmedByReadback(t *testing.T) {
	var mu sync.Mutex
	var uploaded, committed []byte
	var operationID string
	var completeCount, abortCount int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Query().Has("uploads"):
			mu.Lock()
			operationID = request.Header.Get("x-amz-meta-registry-hub-operation")
			mu.Unlock()
			writer.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(writer, `<InitiateMultipartUploadResult><UploadId>upload-unknown</UploadId></InitiateMultipartUploadResult>`)
		case request.Method == http.MethodPut && request.URL.Query().Get("partNumber") == "1":
			part, err := io.ReadAll(request.Body)
			if err != nil {
				http.Error(writer, "read failed", http.StatusBadRequest)
				return
			}
			mu.Lock()
			uploaded = part
			mu.Unlock()
			writer.Header().Set("ETag", `"part-etag"`)
		case request.Method == http.MethodPost && request.URL.Query().Get("uploadId") == "upload-unknown":
			mu.Lock()
			completeCount++
			committed = append([]byte(nil), uploaded...)
			mu.Unlock()
			writer.Header().Set("Content-Type", "application/xml")
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(writer, `<Error><Code>InternalError</Code><Message>response lost after commit</Message></Error>`)
		case request.Method == http.MethodHead:
			mu.Lock()
			length := len(committed)
			id := operationID
			mu.Unlock()
			if length == 0 {
				http.NotFound(writer, request)
				return
			}
			writer.Header().Set("ETag", `"committed-etag"`)
			writer.Header().Set("Content-Length", fmt.Sprint(length))
			writer.Header().Set("x-amz-meta-registry-hub-operation", id)
		case request.Method == http.MethodGet:
			mu.Lock()
			data := append([]byte(nil), committed...)
			id := operationID
			mu.Unlock()
			writer.Header().Set("ETag", `"committed-etag"`)
			writer.Header().Set("Content-Length", fmt.Sprint(len(data)))
			writer.Header().Set("x-amz-meta-registry-hub-operation", id)
			_, _ = writer.Write(data)
		case request.Method == http.MethodDelete && request.URL.Query().Get("uploadId") == "upload-unknown":
			mu.Lock()
			abortCount++
			mu.Unlock()
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := s3.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test-key", "test-secret", ""),
		Retryer:     func() aws.Retryer { return aws.NopRetryer{} },
	}, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(server.URL)
		options.UsePathStyle = true
	})
	store := &Store{client: client, bucket: "registry-hub", multipartThreshold: 1, partSize: 5 << 20}
	defer store.Close()
	data := bytes.Repeat([]byte("x"), 5<<20)
	entry, err := store.Put(context.Background(), "test/multipart-unknown", bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err != nil {
		t.Fatalf("multipart Put after confirmed unknown commit: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if operationID == "" || entry.Version != `"committed-etag"` || entry.Size != int64(len(data)) || !bytes.Equal(committed, data) {
		t.Fatalf("confirmed multipart entry=%#v committedBytes=%d", entry, len(committed))
	}
	if completeCount != 1 || abortCount != 0 {
		t.Fatalf("unknown multipart commit complete=%d abort=%d, want one complete and no abort", completeCount, abortCount)
	}
}

func TestUnknownPutReadbackRequiresMatchingOperationIdentity(t *testing.T) {
	data := []byte("same")
	digest := sha256.Sum256(data)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("ETag", `"same-etag"`)
		writer.Header().Set("Content-Length", fmt.Sprint(len(data)))
		if request.Method == http.MethodHead {
			writer.Header().Set("x-amz-meta-registry-hub-operation", "expected-operation")
			return
		}
		if request.Method == http.MethodGet {
			writer.Header().Set("x-amz-meta-registry-hub-operation", "different-operation")
			_, _ = writer.Write(data)
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()
	client := s3.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test-key", "test-secret", ""),
		Retryer:     func() aws.Retryer { return aws.NopRetryer{} },
	}, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(server.URL)
		options.UsePathStyle = true
	})
	store := &Store{client: client, bucket: "registry-hub"}
	defer store.Close()

	_, matched, err := store.resolvePut(context.Background(), "test/unknown-operation", int64(len(data)), digest[:], "expected-operation")
	if err != nil {
		t.Fatalf("resolvePut() error = %v", err)
	}
	if matched {
		t.Fatal("resolvePut() accepted matching bytes and ETag with a different operation identity")
	}
}

func TestUnknownPutReadbackRejectsObjectReplacedAfterHead(t *testing.T) {
	data := []byte("same bytes, different object version")
	digest := sha256.Sum256(data)
	var getCount int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Length", fmt.Sprint(len(data)))
		writer.Header().Set("x-amz-meta-registry-hub-operation", "expected-operation")
		switch request.Method {
		case http.MethodHead:
			writer.Header().Set("ETag", `"head-version"`)
		case http.MethodGet:
			getCount++
			if request.Header.Get("If-Match") != `"head-version"` {
				http.Error(writer, "GET was not pinned to HEAD version", http.StatusBadRequest)
				return
			}
			writer.Header().Set("ETag", `"replacement-version"`)
			_, _ = writer.Write(data)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := s3.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test-key", "test-secret", ""),
		Retryer:     func() aws.Retryer { return aws.NopRetryer{} },
	}, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(server.URL)
		options.UsePathStyle = true
	})
	store := &Store{client: client, bucket: "registry-hub"}
	defer store.Close()

	_, matched, err := store.resolvePut(context.Background(), "test/replaced-after-head", int64(len(data)), digest[:], "expected-operation")
	if err != nil {
		t.Fatalf("resolvePut() error = %v", err)
	}
	if matched || getCount != 1 {
		t.Fatalf("resolvePut() matched=%t GET count=%d, want a rejected replacement after one version-pinned read", matched, getCount)
	}
}
