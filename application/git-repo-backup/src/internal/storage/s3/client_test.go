package s3

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"git-repo-backup/internal/config"
)

func TestNewClientUsesHTTPBaseEndpoint(t *testing.T) {
	requests := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	credentialsDir := t.TempDir()
	for name, value := range map[string]string{
		accessKeyFile: "access",
		secretKeyFile: "secret",
	} {
		if err := os.WriteFile(filepath.Join(credentialsDir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	client, err := newClient(context.Background(), config.S3Storage{
		Endpoint:          server.URL,
		AllowInsecureHTTP: true,
		Region:            "us-east-1",
		Bucket:            "backups",
		CredentialsMode:   "secret",
		CredentialsDir:    credentialsDir,
		ForcePathStyle:    true,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String("backups")}); err != nil {
		t.Fatal(err)
	}

	select {
	case request := <-requests:
		if request.TLS != nil {
			t.Fatal("HTTP endpoint request unexpectedly used TLS")
		}
		if request.URL.Path != "/backups" {
			t.Fatalf("request path = %q, want /backups", request.URL.Path)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for HTTP S3 request")
	}
}
