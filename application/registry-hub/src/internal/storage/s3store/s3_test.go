package s3store

import (
	"bytes"
	"context"
	"errors"
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

func TestPutReportsUnknownCommitWhenConfirmationReadFails(t *testing.T) {
	var mu sync.Mutex
	var committed []byte
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodPut:
			body, err := io.ReadAll(request.Body)
			if err != nil {
				http.Error(writer, "read failed", http.StatusBadRequest)
				return
			}
			mu.Lock()
			committed = append([]byte(nil), body...)
			mu.Unlock()
			writer.Header().Set("Content-Type", "application/xml")
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(writer, `<Error><Code>InternalError</Code><Message>response lost after commit</Message></Error>`)
		case http.MethodHead:
			writer.WriteHeader(http.StatusServiceUnavailable)
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
	store := &Store{client: client, bucket: "registry-hub", multipartThreshold: 1 << 20}
	data := []byte("committed despite failed response")
	_, err := store.Put(context.Background(), "test/object", bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if !errors.Is(err, storage.ErrUnknownCommit) {
		t.Fatalf("Put error = %v, want ErrUnknownCommit", err)
	}
	if !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("Put error = %v, want to preserve ErrUnavailable classification", err)
	}
	mu.Lock()
	got := append([]byte(nil), committed...)
	mu.Unlock()
	if !bytes.Equal(got, data) {
		t.Fatalf("server committed %q, want %q", got, data)
	}
}

func TestMultipartPutUsesConditionalComplete(t *testing.T) {
	var mu sync.Mutex
	var uploadedPart []byte
	var createCount, partCount, completeCount int
	var completeHadIfNoneMatch bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Query().Has("uploads"):
			mu.Lock()
			createCount++
			mu.Unlock()
			writer.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(writer, `<InitiateMultipartUploadResult><UploadId>upload-1</UploadId></InitiateMultipartUploadResult>`)
		case request.Method == http.MethodPut && request.URL.Query().Get("partNumber") == "1":
			body, err := io.ReadAll(request.Body)
			if err != nil {
				http.Error(writer, "read failed", http.StatusBadRequest)
				return
			}
			mu.Lock()
			uploadedPart = body
			partCount++
			mu.Unlock()
			writer.Header().Set("ETag", `"part-etag"`)
		case request.Method == http.MethodPost && request.URL.Query().Get("uploadId") == "upload-1":
			mu.Lock()
			completeCount++
			completeHadIfNoneMatch = request.Header.Get("If-None-Match") == "*"
			mu.Unlock()
			writer.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(writer, `<CompleteMultipartUploadResult><ETag>"complete-etag"</ETag></CompleteMultipartUploadResult>`)
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
	store := &Store{client: client, bucket: "registry-hub", multipartThreshold: 5 << 20, partSize: 5 << 20}
	data := bytes.Repeat([]byte("x"), 5<<20)
	entry, err := store.Put(context.Background(), "test/multipart", bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err != nil {
		t.Fatalf("multipart Put: %v", err)
	}
	if entry.Version != `"complete-etag"` || entry.Size != int64(len(data)) {
		t.Fatalf("multipart entry = %#v, want complete ETag and full size", entry)
	}
	mu.Lock()
	gotPart := len(uploadedPart)
	gotCreates, gotParts, gotCompletes, gotCondition := createCount, partCount, completeCount, completeHadIfNoneMatch
	mu.Unlock()
	if gotCreates != 1 || gotParts != 1 || gotCompletes != 1 || !gotCondition || gotPart != len(data) {
		t.Fatalf("multipart calls create=%d part=%d complete=%d condition=%t partBytes=%d", gotCreates, gotParts, gotCompletes, gotCondition, gotPart)
	}
}

func TestMultipartConditionalConflictAbortsUpload(t *testing.T) {
	var mu sync.Mutex
	var completeCount, abortCount int
	var completeHadIfNoneMatch bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Query().Has("uploads"):
			writer.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(writer, `<InitiateMultipartUploadResult><UploadId>upload-conflict</UploadId></InitiateMultipartUploadResult>`)
		case request.Method == http.MethodPut && request.URL.Query().Get("partNumber") == "1":
			_, _ = io.Copy(io.Discard, request.Body)
			writer.Header().Set("ETag", `"part-etag"`)
		case request.Method == http.MethodPost && request.URL.Query().Get("uploadId") == "upload-conflict":
			mu.Lock()
			completeCount++
			completeHadIfNoneMatch = request.Header.Get("If-None-Match") == "*"
			mu.Unlock()
			writer.Header().Set("Content-Type", "application/xml")
			writer.WriteHeader(http.StatusPreconditionFailed)
			_, _ = io.WriteString(writer, `<Error><Code>PreconditionFailed</Code></Error>`)
		case request.Method == http.MethodDelete && request.URL.Query().Get("uploadId") == "upload-conflict":
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
	_, err := store.Put(context.Background(), "test/multipart-conflict", bytes.NewReader(bytes.Repeat([]byte("x"), 5<<20)), 5<<20, storage.PutOptions{IfAbsent: true})
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("multipart conditional conflict = %v, want ErrAlreadyExists", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if completeCount != 1 || abortCount != 1 || !completeHadIfNoneMatch {
		t.Fatalf("multipart conflict complete=%d abort=%d If-None-Match=%t", completeCount, abortCount, completeHadIfNoneMatch)
	}
}
