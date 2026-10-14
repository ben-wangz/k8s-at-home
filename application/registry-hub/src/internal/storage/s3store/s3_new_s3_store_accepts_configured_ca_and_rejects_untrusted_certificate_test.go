package s3store

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"registry-hub/internal/config"
	"registry-hub/internal/storage"
	"testing"
	"time"
)

func TestNewS3StoreAcceptsConfiguredCAAndRejectsUntrustedCertificate(t *testing.T) {
	var putCount int
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPut {
			http.NotFound(writer, request)
			return
		}
		_, _ = io.Copy(io.Discard, request.Body)
		putCount++
		writer.Header().Set("ETag", `"tls-etag"`)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	caFile := t.TempDir() + "/s3-ca.crt"
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	base := config.S3{Endpoint: server.URL, Region: "us-east-1", Bucket: "registry-hub", UsePathStyle: true, Timeout: 5 * time.Second, MaxConnections: 1, MultipartThreshold: 5 << 20}
	trustedConfig := base
	trustedConfig.CAFile = caFile
	trusted, err := New(context.Background(), trustedConfig)
	if err != nil {
		t.Fatalf("create S3 store with fixture CA: %v", err)
	}
	entry, err := trusted.Put(context.Background(), "test/trusted", bytes.NewReader([]byte("trusted")), int64(len("trusted")), storage.PutOptions{IfAbsent: true})
	if err != nil || entry.Version != `"tls-etag"` {
		t.Fatalf("Put using configured CA = entry %#v, error %v", entry, err)
	}
	_ = trusted.Close()

	untrusted, err := New(context.Background(), base)
	if err != nil {
		t.Fatalf("create S3 store without fixture CA: %v", err)
	}
	defer untrusted.Close()
	if _, err := untrusted.Put(context.Background(), "test/untrusted", bytes.NewReader([]byte("untrusted")), int64(len("untrusted")), storage.PutOptions{IfAbsent: true}); err == nil {
		t.Fatal("S3 request accepted a certificate absent from system and configured roots")
	}
	if putCount != 1 {
		t.Fatalf("S3 PUT requests reaching the TLS fixture = %d, want only trusted request", putCount)
	}
}

func TestNewHTTPPutSupportsUnseekableStreams(t *testing.T) {
	var gotBody []byte
	var gotChecksumAlgorithm, gotTrailer, gotPayloadHash string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPut {
			http.NotFound(writer, request)
			return
		}
		gotBody, _ = io.ReadAll(request.Body)
		gotChecksumAlgorithm = request.Header.Get("x-amz-sdk-checksum-algorithm")
		gotTrailer = request.Header.Get("x-amz-trailer")
		gotPayloadHash = request.Header.Get("x-amz-content-sha256")
		writer.Header().Set("ETag", `"http-put-etag"`)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_REQUEST_CHECKSUM_CALCULATION", "when_supported")
	store, err := New(context.Background(), config.S3{
		Endpoint: server.URL, AllowHTTP: true, Region: "us-east-1", Bucket: "registry-hub",
		MultipartThreshold: 1 << 20, PartSize: 5 << 20, Timeout: 5 * time.Second, MaxConnections: 1,
	})
	if err != nil {
		t.Fatalf("create S3 store for explicit HTTP endpoint: %v", err)
	}
	defer store.Close()

	payload := []byte("non-seekable HTTP payload")
	entry, err := store.Put(context.Background(), "test/object", bytes.NewReader(payload), int64(len(payload)), storage.PutOptions{IfAbsent: true})
	if err != nil {
		t.Fatalf("HTTP Put: %v", err)
	}
	if entry.Version != `"http-put-etag"` || entry.Size != int64(len(payload)) {
		t.Fatalf("HTTP Put entry = %#v, want ETag and payload size", entry)
	}
	if !bytes.Equal(gotBody, payload) {
		t.Fatalf("HTTP Put body = %q, want %q", gotBody, payload)
	}
	if gotChecksumAlgorithm != "" || gotTrailer != "" || gotPayloadHash != "UNSIGNED-PAYLOAD" {
		t.Fatalf("HTTP Put checksum headers algorithm=%q trailer=%q payloadHash=%q", gotChecksumAlgorithm, gotTrailer, gotPayloadHash)
	}
}

func TestDeleteIfMatchMapsS3PreconditionFailure(t *testing.T) {
	var gotIfMatch string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodDelete {
			http.NotFound(writer, request)
			return
		}
		gotIfMatch = request.Header.Get("If-Match")
		writer.Header().Set("Content-Type", "application/xml")
		writer.WriteHeader(http.StatusPreconditionFailed)
		_, _ = io.WriteString(writer, `<Error><Code>PreconditionFailed</Code></Error>`)
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
	err := store.DeleteIfMatch(context.Background(), "test/object", `"old-version"`)
	if !errors.Is(err, storage.ErrConditionFailed) {
		t.Fatalf("DeleteIfMatch error = %v, want ErrConditionFailed", err)
	}
	if gotIfMatch != `"old-version"` {
		t.Fatalf("If-Match = %q, want the requested version", gotIfMatch)
	}
}

func TestConditionalCreateMapsS3PreconditionFailureToAlreadyExists(t *testing.T) {
	var gotIfNoneMatch string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPut {
			http.NotFound(writer, request)
			return
		}
		gotIfNoneMatch = request.Header.Get("If-None-Match")
		writer.Header().Set("Content-Type", "application/xml")
		writer.WriteHeader(http.StatusPreconditionFailed)
		_, _ = io.WriteString(writer, `<Error><Code>PreconditionFailed</Code></Error>`)
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
	payload := []byte("already exists")
	_, err := store.Put(context.Background(), "test/object", bytes.NewReader(payload), int64(len(payload)), storage.PutOptions{IfAbsent: true})
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("conditional create error = %v, want ErrAlreadyExists", err)
	}
	if gotIfNoneMatch != "*" {
		t.Fatalf("If-None-Match = %q, want *", gotIfNoneMatch)
	}
}
