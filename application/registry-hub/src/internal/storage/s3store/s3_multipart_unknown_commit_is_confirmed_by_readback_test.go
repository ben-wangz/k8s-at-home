package s3store

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"registry-hub/internal/config"
	"registry-hub/internal/storage"
	"sync"
	"testing"
	"time"
)

func TestMultipartUnknownCommitIsConfirmedByReadback(t *testing.T) {
	var mu sync.Mutex
	var uploaded, committed []byte
	var completeCount, abortCount int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Query().Has("uploads"):
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
			mu.Unlock()
			if length == 0 {
				http.NotFound(writer, request)
				return
			}
			writer.Header().Set("ETag", `"committed-etag"`)
			writer.Header().Set("Content-Length", fmt.Sprint(length))
		case request.Method == http.MethodGet:
			mu.Lock()
			data := append([]byte(nil), committed...)
			mu.Unlock()
			writer.Header().Set("ETag", `"committed-etag"`)
			writer.Header().Set("Content-Length", fmt.Sprint(len(data)))
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
	data := bytes.Repeat([]byte("x"), 5<<20)
	entry, err := store.Put(context.Background(), "test/multipart-unknown", bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err != nil {
		t.Fatalf("multipart Put after confirmed unknown commit: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if entry.Version != `"committed-etag"` || entry.Size != int64(len(data)) || !bytes.Equal(committed, data) {
		t.Fatalf("confirmed multipart entry=%#v committedBytes=%d", entry, len(committed))
	}
	if completeCount != 1 || abortCount != 0 {
		t.Fatalf("unknown multipart commit complete=%d abort=%d, want one complete and no abort", completeCount, abortCount)
	}
}

func TestConcurrentConditionalCreatesHaveSingleWinner(t *testing.T) {
	var mu sync.Mutex
	var object []byte
	var puts int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPut {
			http.NotFound(writer, request)
			return
		}
		data, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, "read failed", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		puts++
		if request.Header.Get("If-None-Match") != "*" {
			http.Error(writer, "missing conditional header", http.StatusBadRequest)
			return
		}
		if object != nil {
			writer.Header().Set("Content-Type", "application/xml")
			writer.WriteHeader(http.StatusPreconditionFailed)
			_, _ = io.WriteString(writer, `<Error><Code>PreconditionFailed</Code></Error>`)
			return
		}
		object = append([]byte(nil), data...)
		writer.Header().Set("ETag", `"winner-etag"`)
		writer.WriteHeader(http.StatusOK)
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
	type result struct {
		payload []byte
		err     error
	}
	payloads := [][]byte{[]byte("first conditional value"), []byte("second conditional value")}
	start := make(chan struct{})
	results := make(chan result, len(payloads))
	for _, payload := range payloads {
		go func(data []byte) {
			<-start
			_, err := store.Put(context.Background(), "test/concurrent", bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
			results <- result{payload: data, err: err}
		}(payload)
	}
	close(start)
	winners := 0
	losers := 0
	var expected []byte
	for range payloads {
		result := <-results
		switch {
		case result.err == nil:
			winners++
			expected = result.payload
		case errors.Is(result.err, storage.ErrAlreadyExists):
			losers++
		default:
			t.Fatalf("conditional create error = %v", result.err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if winners != 1 || losers != 1 || puts != 2 || !bytes.Equal(object, expected) {
		t.Fatalf("conditional create winners=%d losers=%d requests=%d object=%q want=%q", winners, losers, puts, object, expected)
	}
}

func TestNewRequiresExplicitHTTPAndUsesVerifiedTLSRoots(t *testing.T) {
	if err := validateEndpoint("http://s3.example.test", false); err == nil {
		t.Fatal("HTTP endpoint was accepted without explicit opt-in")
	}
	if _, err := New(context.Background(), config.S3{Endpoint: "http://s3.example.test", Bucket: "registry-hub"}); err == nil {
		t.Fatal("New accepted an HTTP endpoint without explicit opt-in")
	}
	if err := validateEndpoint("ftp://s3.example.test", true); err == nil {
		t.Fatal("non-HTTP endpoint was accepted")
	}
	if err := validateEndpoint("http://s3.example.test", true); err != nil {
		t.Fatalf("explicit HTTP endpoint rejected: %v", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	root := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "registry-hub test CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caFile := t.TempDir() + "/ca.crt"
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	store, err := New(context.Background(), config.S3{
		Endpoint: "https://s3.example.test", Bucket: "registry-hub", CAFile: caFile,
		MultipartThreshold: 5 << 20, PartSize: 5 << 20, Timeout: time.Second, MaxConnections: 1,
	})
	if err != nil {
		t.Fatalf("create S3 store with a custom CA: %v", err)
	}
	defer store.Close()
	transport := store.httpClient.Transport.(*http.Transport)
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Fatalf("S3 TLS config is not verified TLS 1.2+: %#v", transport.TLSClientConfig)
	}
	if transport.TLSClientConfig.RootCAs == nil {
		t.Fatal("custom CA was not installed in the S3 TLS root pool")
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: transport.TLSClientConfig.RootCAs, CurrentTime: now}); err != nil {
		t.Fatalf("custom CA is not trusted by S3 transport: %v", err)
	}
}
