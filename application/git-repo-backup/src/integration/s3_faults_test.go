//go:build integration

// S3 fault injection: lost commit-marker responses, concurrent claims on
// the same backup ID, and TLS verification without a trusted CA.
package integration

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git-repo-backup/internal/config"
	"git-repo-backup/internal/observability"
	"git-repo-backup/internal/storage"
	s3storage "git-repo-backup/internal/storage/s3"
)

// TestS3MarkerResponseLost proves the response-lost protocol: the proxy
// applies the _SUCCESS PUT at the service and then hides the response. The
// run must reconcile by re-reading the marker and exit successfully —
// never report failure-with-possible-data, never delete the backup.
func TestS3MarkerResponseLost(t *testing.T) {
	canRunRooted(t)
	f := s3Fixture(t)
	client := fixtureClient(t, f)
	proxy, proxyEndpoint := startRecordingProxy(t, f)
	proxy.dropMarkerResponse.Store(true)

	dir := t.TempDir()
	srv := startSSHGitServer(t, dir)
	bin := buildBinary(t)
	origin := seedSourceRepo(t, dir, "alpha")
	prefix := fmt.Sprintf("it-drop-%d", time.Now().UnixNano())
	credentialsDir := writeFixtureCredentials(t, dir, f)
	repos := fmt.Sprintf("repositories:\n  - name: alpha\n    url: ssh://root@127.0.0.1:%d%s\n", srv.port, origin)
	cfg := writeS3Config(t, s3RunConfig{
		dir: dir, endpoint: proxyEndpoint, caFile: proxy.caFile,
		credentials: credentialsDir, bucket: f.bucket, prefix: prefix,
		workspace: filepath.Join(dir, "workspace"), reposYAML: repos,
		maxBackups: 5, retentionOn: true,
	})
	code, out := runBackup(t, bin, cfg)
	if code != 0 {
		t.Fatalf("reconciled run must succeed, got %d:\n%s", code, out)
	}
	backupID := backupIDOf(t, client, f, prefix)
	if err := headOK(t, client, f.bucket, fmt.Sprintf("%s/%s/_SUCCESS", prefix, backupID)); err != nil {
		t.Fatalf("marker must exist after reconciled commit: %v", err)
	}
	// The dropped response must actually have been exercised.
	dropped := false
	for _, r := range proxy.snapshot() {
		if r.Method == "PUT" && strings.HasSuffix(r.Key, "/_SUCCESS") && r.Status != 0 {
			dropped = true
		}
	}
	if !dropped {
		t.Fatal("marker PUT was not forwarded (fault not exercised)")
	}
}

// TestS3ClaimRace drives two stores with different owners against the same
// backup ID: exactly one claim wins, and every later process fails closed
// because it cannot safely establish ownership of the existing claim.
func TestS3ClaimRace(t *testing.T) {
	canRunRooted(t)
	f := s3Fixture(t)
	dir := t.TempDir()
	credentialsDir := writeFixtureCredentials(t, dir, f)
	prefix := fmt.Sprintf("it-race-%d", time.Now().UnixNano())
	cfg := config.S3Storage{
		Endpoint: f.endpoint, Region: "us-east-1", Bucket: f.bucket, Prefix: prefix,
		ForcePathStyle: true, CredentialsMode: "secret",
		CredentialsDir: credentialsDir, CABundleFile: f.caPath,
	}
	ctx := context.Background()
	newStore := func() *s3storage.Store {
		st, err := s3storage.New(ctx, cfg, slog.Default())
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	id := storage.RunIdentity{
		BackupID:  storage.FormatBackupID(time.Now().UTC()),
		OwnerUUID: "owner-a",
		StartedAt: time.Now().UTC(),
	}
	if err := newStore().Begin(ctx, id); err != nil {
		t.Fatalf("first claim must win: %v", err)
	}
	loser := id
	loser.OwnerUUID = "owner-b"
	if err := newStore().Begin(ctx, loser); err == nil {
		t.Fatal("second owner must not take over the claim")
	} else if observability.CodeOf(err) != observability.CodePublishConflict {
		t.Fatalf("expected publish conflict, got %v", err)
	}
	// A later process must not infer ownership from matching claim bytes.
	if err := newStore().Begin(ctx, id); err == nil {
		t.Fatal("existing claim must fail closed")
	} else if observability.CodeOf(err) != observability.CodePublishConflict {
		t.Fatalf("expected publish conflict for existing claim, got %v", err)
	}
}

// TestS3UntrustedCAFails points the binary at the TLS fixture without the
// CA bundle: the run must fail before any object is written, and the
// independent verification client (which does trust the CA) observes an
// empty prefix.
func TestS3UntrustedCAFails(t *testing.T) {
	canRunRooted(t)
	f := s3Fixture(t)
	client := fixtureClient(t, f)

	dir := t.TempDir()
	srv := startSSHGitServer(t, dir)
	bin := buildBinary(t)
	origin := seedSourceRepo(t, dir, "alpha")
	prefix := fmt.Sprintf("it-noca-%d", time.Now().UnixNano())
	credentialsDir := writeFixtureCredentials(t, dir, f)
	repos := fmt.Sprintf("repositories:\n  - name: alpha\n    url: ssh://root@127.0.0.1:%d%s\n", srv.port, origin)
	cfg := writeS3Config(t, s3RunConfig{
		dir: dir, endpoint: f.endpoint, caFile: "",
		credentials: credentialsDir, bucket: f.bucket, prefix: prefix,
		workspace: filepath.Join(dir, "workspace"), reposYAML: repos,
		maxBackups: 1, retentionOn: false,
	})
	code, out := runBackup(t, bin, cfg)
	if code == 0 {
		t.Fatal("run must fail against a TLS endpoint whose CA is not trusted")
	}
	if !strings.Contains(out, "storage_failed") || strings.Contains(out, "config_invalid") {
		t.Fatalf("run must fail during TLS-backed storage access, not config parsing:\n%s", out)
	}
	if keys := listAll(t, client, f.bucket, prefix+"/"); len(keys) != 0 {
		t.Fatalf("no objects may be written when TLS fails, got %v", keys)
	}
}

// writeFixtureCredentials materializes the fixture static credentials in
// the on-disk layout the program expects.
func writeFixtureCredentials(t *testing.T, dir string, f s3FixtureInfo) string {
	t.Helper()
	credentialsDir := filepath.Join(dir, "s3creds")
	if err := os.MkdirAll(credentialsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"access-key-id": f.accessKey, "secret-access-key": f.secretKey,
	} {
		if err := os.WriteFile(filepath.Join(credentialsDir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return credentialsDir
}
