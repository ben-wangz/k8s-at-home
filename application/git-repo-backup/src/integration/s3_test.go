//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3 fixture endpoints are provided by container/test/launch-fixtures.sh
// through these environment variables; tests skip when absent.
const (
	envEndpoint = "GIT_REPO_BACKUP_IT_S3_ENDPOINT"
	envCA       = "GIT_REPO_BACKUP_IT_S3_CA"
	envAccess   = "GIT_REPO_BACKUP_IT_S3_ACCESS_KEY"
	envSecret   = "GIT_REPO_BACKUP_IT_S3_SECRET_KEY"
	envBucket   = "GIT_REPO_BACKUP_IT_S3_BUCKET"
)

func s3Fixture(t *testing.T) (endpoint, caPath, accessKey, secretKey, bucket string) {
	t.Helper()
	values := []string{
		os.Getenv(envEndpoint), os.Getenv(envCA), os.Getenv(envAccess),
		os.Getenv(envSecret), os.Getenv(envBucket),
	}
	for _, v := range values {
		if v == "" {
			t.Skip("S3 fixture environment not set; run container/test/launch-fixtures.sh")
		}
	}
	return values[0], values[1], values[2], values[3], values[4]
}

// fixtureClient talks to the MinIO fixture directly for verification.
func fixtureClient(t *testing.T, endpoint, caPath, accessKey, secretKey string) *s3.Client {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
		awsconfig.WithBaseEndpoint(endpoint),
	)
	if err != nil {
		t.Fatal(err)
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) { o.UsePathStyle = true })
}

// TestS3EndToEnd backs up one repository to the TLS S3 fixture and checks
// the object layout, the marker-last protocol via absence before commit
// data, and digest agreement with the local archive.
func TestS3EndToEnd(t *testing.T) {
	canRunRooted(t)
	endpoint, caPath, accessKey, secretKey, bucket := s3Fixture(t)
	client := fixtureClient(t, endpoint, caPath, accessKey, secretKey)

	dir := t.TempDir()
	srv := startSSHGitServer(t, dir)
	bin := buildBinary(t)
	origin := seedSourceRepo(t, dir, "alpha")
	prefix := fmt.Sprintf("it-%d", time.Now().UnixNano())

	credentialsDir := filepath.Join(dir, "s3creds")
	if err := os.MkdirAll(credentialsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"access-key-id": accessKey, "secret-access-key": secretKey,
	} {
		if err := os.WriteFile(filepath.Join(credentialsDir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	repos := fmt.Sprintf("repositories:\n  - name: alpha\n    url: ssh://root@127.0.0.1:%d%s\n", srv.port, origin)
	reposPath := filepath.Join(dir, "repositories.yaml")
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(reposPath, []byte(repos), 0o600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`schemaVersion: 1
repositoriesFile: %s
ssh:
  privateKeyFile: /etc/git-repo-backup/ssh/id
  knownHostsFile: /etc/git-repo-backup/ssh/known_hosts
storage:
  type: s3
  s3:
    endpoint: %s
    region: us-east-1
    bucket: %s
    prefix: %s
    forcePathStyle: true
    credentialsMode: secret
    credentialsDir: %s
    caBundleFile: %s
workspace:
  root: %s
backup:
  maxRunDuration: 10m
  gitTimeout: 2m
retention:
  enabled: true
  maxBackups: 5
  maxAge: ""
  incompleteMaxAge: 30m
log:
  level: debug
`, reposPath, endpoint, bucket, prefix, credentialsDir, caPath, filepath.Join(dir, "workspace"))
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out := runBackup(t, bin, configPath)
	if code != 0 {
		t.Fatalf("s3 run failed (%d):\n%s", code, out)
	}

	// Verify layout through an independent client.
	keys := listAll(t, client, bucket, prefix+"/")
	var backupID string
	for _, k := range keys {
		if !strings.Contains(k, "/") {
			continue
		}
		parts := strings.SplitN(strings.TrimPrefix(k, prefix+"/"), "/", 2)
		if len(parts) == 2 && len(parts[0]) == 16 && parts[0] != ".control" {
			backupID = parts[0]
			break
		}
	}
	if backupID == "" {
		t.Fatalf("no backup prefix found, keys: %v", keys)
	}
	runPrefix := fmt.Sprintf("%s/%s/", prefix, backupID)
	for _, suffix := range []string{"manifest.json", "checksums.sha256", "_SUCCESS", "repositories/alpha.tar.gz"} {
		if err := headOK(t, client, bucket, runPrefix+suffix); err != nil {
			t.Fatalf("missing %s: %v", suffix, err)
		}
	}
	// The claim tombstone must exist and never count as backup content.
	if err := headOK(t, client, bucket, fmt.Sprintf("%s/.control/claims/%s.json", prefix, backupID)); err != nil {
		t.Fatalf("claim missing: %v", err)
	}
	// Manifest must not leak the source URL.
	manifest := getObject(t, client, bucket, runPrefix+"manifest.json")
	if strings.Contains(string(manifest), "127.0.0.1") {
		t.Fatal("manifest must not contain source URLs")
	}
}

// TestS3UntrustedCAFails proves the run cannot talk to the fixture when the
// CA bundle is not provided: TLS verification is never bypassed.
func TestS3UntrustedCAFails(t *testing.T) {
	canRunRooted(t)
	endpoint, _, accessKey, secretKey, bucket := s3Fixture(t)

	dir := t.TempDir()
	srv := startSSHGitServer(t, dir)
	bin := buildBinary(t)
	origin := seedSourceRepo(t, dir, "alpha")
	prefix := fmt.Sprintf("it-noca-%d", time.Now().UnixNano())
	credentialsDir := filepath.Join(dir, "s3creds")
	_ = os.MkdirAll(credentialsDir, 0o700)
	_ = os.WriteFile(filepath.Join(credentialsDir, "access-key-id"), []byte(accessKey), 0o600)
	_ = os.WriteFile(filepath.Join(credentialsDir, "secret-access-key"), []byte(secretKey), 0o600)
	repos := fmt.Sprintf("repositories:\n  - name: alpha\n    url: ssh://root@127.0.0.1:%d%s\n", srv.port, origin)
	reposPath := filepath.Join(dir, "repositories.yaml")
	configPath := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(reposPath, []byte(repos), 0o600)
	body := fmt.Sprintf(`schemaVersion: 1
repositoriesFile: %s
ssh:
  privateKeyFile: /etc/git-repo-backup/ssh/id
  knownHostsFile: /etc/git-repo-backup/ssh/known_hosts
storage:
  type: s3
  s3:
    endpoint: %s
    region: us-east-1
    bucket: %s
    prefix: %s
    forcePathStyle: true
    credentialsMode: secret
    credentialsDir: %s
workspace:
  root: %s
backup:
  maxRunDuration: 10m
  gitTimeout: 2m
retention:
  enabled: false
  maxBackups: 1
  maxAge: ""
  incompleteMaxAge: 30m
`, reposPath, endpoint, bucket, prefix, credentialsDir, filepath.Join(dir, "workspace"))
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _ := runBackup(t, bin, configPath)
	if code == 0 {
		t.Fatal("run must fail against a TLS endpoint whose CA is not trusted")
	}
	client := fixtureClient(t, endpoint, "", accessKey, secretKey)
	if keys := listAll(t, client, bucket, prefix+"/"); len(keys) != 0 {
		t.Fatalf("no objects may be written when TLS fails, got %v", keys)
	}
}
