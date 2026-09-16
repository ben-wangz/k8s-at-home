//go:build integration

// SDK helpers for verifying fixture state independently of the backup
// binary. The verification client must itself trust the fixture CA: only
// the program under test may be given a missing or wrong CA in negative
// cases.
package integration

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"git-repo-backup/internal/manifest"
)

// S3 fixture endpoints are provided by container/test/run-integration.sh
// through these environment variables; tests skip when absent.
const (
	envEndpoint = "GIT_REPO_BACKUP_IT_S3_ENDPOINT"
	envCA       = "GIT_REPO_BACKUP_IT_S3_CA"
	envAccess   = "GIT_REPO_BACKUP_IT_S3_ACCESS_KEY"
	envSecret   = "GIT_REPO_BACKUP_IT_S3_SECRET_KEY"
	envBucket   = "GIT_REPO_BACKUP_IT_S3_BUCKET"
)

type s3FixtureInfo struct {
	endpoint  string
	caPath    string
	accessKey string
	secretKey string
	bucket    string
}

func s3Fixture(t *testing.T) s3FixtureInfo {
	t.Helper()
	f := s3FixtureInfo{
		endpoint:  os.Getenv(envEndpoint),
		caPath:    os.Getenv(envCA),
		accessKey: os.Getenv(envAccess),
		secretKey: os.Getenv(envSecret),
		bucket:    os.Getenv(envBucket),
	}
	for _, v := range []string{f.endpoint, f.caPath, f.accessKey, f.secretKey, f.bucket} {
		if v == "" {
			t.Skip("S3 fixture environment not set; run container/test/run-integration.sh all so the runner injects the fixture")
		}
	}
	return f
}

// fixtureClient talks to the MinIO fixture directly for verification. When
// caPath is set it is APPENDED to the system trust pool of this client so
// verification stays strict; TLS is never disabled here.
func fixtureClient(t *testing.T, f s3FixtureInfo) *s3.Client {
	t.Helper()
	var httpClient *awshttp.BuildableClient
	if f.caPath != "" {
		pem, err := os.ReadFile(f.caPath)
		if err != nil {
			t.Fatalf("read fixture CA: %v", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			t.Fatal(err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			t.Fatal("fixture CA contains no certificates")
		}
		httpClient = awshttp.NewBuildableClient().WithTransportOptions(func(tr *http.Transport) {
			tr.TLSClientConfig = &tls.Config{RootCAs: pool}
		})
	}
	var opts []func(*awsconfig.LoadOptions) error
	opts = append(opts,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(f.accessKey, f.secretKey, "")),
		awsconfig.WithBaseEndpoint(f.endpoint),
	)
	if httpClient != nil {
		opts = append(opts, awsconfig.WithHTTPClient(httpClient))
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) { o.UsePathStyle = true })
}

func listAll(t *testing.T, client *s3.Client, bucket, prefix string) []string {
	t.Helper()
	var out []string
	pag := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: &bucket, Prefix: &prefix,
	})
	for pag.HasMorePages() {
		page, err := pag.NextPage(context.Background())
		if err != nil {
			t.Fatalf("list %s: %v", prefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key != nil {
				out = append(out, *obj.Key)
			}
		}
	}
	return out
}

func headOK(t *testing.T, client *s3.Client, bucket, key string) error {
	t.Helper()
	_, err := client.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	return err
}

func getObject(t *testing.T, client *s3.Client, bucket, key string) []byte {
	t.Helper()
	out, err := client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer out.Body.Close()
	data, err := io.ReadAll(io.LimitReader(out.Body, 8<<20))
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return data
}

// downloadToFile streams an object to a local file without holding it in
// memory, for digest verification of large archives.
func downloadToFile(t *testing.T, client *s3.Client, bucket, key, dest string) {
	t.Helper()
	out, err := client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer out.Body.Close()
	f, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(f, out.Body); err != nil {
		f.Close()
		t.Fatalf("stream %s: %v", key, err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func filterRecs(recs []recordedRequest, pred func(recordedRequest) bool) []recordedRequest {
	var out []recordedRequest
	for _, r := range recs {
		if pred(r) {
			out = append(out, r)
		}
	}
	return out
}

// backupIDOf finds the run namespace under the test prefix.
func backupIDOf(t *testing.T, client *s3.Client, f s3FixtureInfo, prefix string) string {
	t.Helper()
	for _, k := range listAll(t, client, f.bucket, prefix+"/") {
		parts := strings.SplitN(strings.TrimPrefix(k, prefix+"/"), "/", 2)
		if len(parts) == 2 && len(parts[0]) == 16 && parts[0] != ".control" {
			return parts[0]
		}
	}
	t.Fatal("no backup prefix found")
	return ""
}

// verifyDownloadedRun reproduces the local restore checks on a downloaded
// run directory: checksums, fsck, refs/OID equality, and marker digest
// agreement computed independently of the program.
func verifyDownloadedRun(t *testing.T, restoreDir, origin string) {
	t.Helper()
	if out := runCmd(t, restoreDir, nil, "sha256sum", "--check", "checksums.sha256"); !strings.Contains(out, "OK") {
		t.Fatalf("downloaded checksum verification failed:\n%s", out)
	}
	runCmd(t, restoreDir, nil, "tar", "-xzf",
		filepath.Join(restoreDir, "repositories", "alpha.tar.gz"), "--no-same-owner")
	mirror := filepath.Join(restoreDir, "alpha.git")
	runCmd(t, mirror, nil, "git", "-C", mirror, "fsck", "--full")
	if !sameStringSlices(refsAndOIDs(t, restoreDir, mirror), refsAndOIDs(t, restoreDir, origin)) {
		t.Fatal("restored refs/OIDs differ from source")
	}
	manifestBytes := mustRead(t, filepath.Join(restoreDir, "manifest.json"))
	checksumsBytes := mustRead(t, filepath.Join(restoreDir, "checksums.sha256"))
	mDigest := sha256.Sum256(manifestBytes)
	cDigest := sha256.Sum256(checksumsBytes)
	marker, err := manifest.ParseMarker(mustRead(t, filepath.Join(restoreDir, "_SUCCESS")))
	if err != nil {
		t.Fatal(err)
	}
	if marker.ManifestSHA256 != hex.EncodeToString(mDigest[:]) ||
		marker.ChecksumsSHA256 != hex.EncodeToString(cDigest[:]) {
		t.Fatal("marker digests do not match downloaded metadata")
	}
	if strings.Contains(string(manifestBytes), "127.0.0.1") {
		t.Fatal("manifest must not contain source URLs")
	}
}
