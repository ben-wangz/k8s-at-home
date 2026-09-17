//go:build integration

// S3 multipart, pagination, and signal coverage: large incompressible
// repositories must use the explicit multipart protocol; more than one
// listing page of objects must be cleaned completely; SIGTERM during an
// upload must abort in-flight parts and never write the marker.
package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// TestS3MultipartLargeRepo archives ~80 MiB of random data: gzip cannot
// shrink it below the 64 MiB part threshold, so the explicit multipart
// protocol must be observable and the completion conditional.
func TestS3MultipartLargeRepo(t *testing.T) {
	canRunRooted(t)
	f := s3Fixture(t)
	client := fixtureClient(t, f)
	proxy, proxyEndpoint := startRecordingProxy(t, f)

	dir := t.TempDir()
	srv := startSSHGitServer(t, dir)
	bin := buildBinary(t)
	origin := seedIncompressibleRepo(t, dir, "big", 80)
	prefix := fmt.Sprintf("it-mp-%d", time.Now().UnixNano())
	credentialsDir := writeFixtureCredentials(t, dir, f)
	repos := fmt.Sprintf("repositories:\n  - name: big\n    url: ssh://root@127.0.0.1:%d%s\n", srv.port, origin)
	cfg := writeS3Config(t, s3RunConfig{
		dir: dir, endpoint: proxyEndpoint, caFile: proxy.caFile,
		credentials: credentialsDir, bucket: f.bucket, prefix: prefix,
		workspace: filepath.Join(dir, "workspace"), reposYAML: repos,
		maxBackups: 5, retentionOn: true,
	})
	if code, out := runBackup(t, bin, cfg); code != 0 {
		t.Fatalf("multipart run failed (%d):\n%s", code, out)
	}
	var creates, completes, parts int
	for _, r := range proxy.snapshot() {
		isArchiveKey := strings.Contains(r.Key, "/repositories/big.tar.gz")
		switch {
		case r.Method == "POST" && strings.HasPrefix(r.Query, "uploads") && isArchiveKey:
			creates++
		case r.Method == "PUT" && strings.Contains(r.Query, "uploadId=") && isArchiveKey:
			parts++
		case r.Method == "POST" && strings.Contains(r.Query, "uploadId=") && isArchiveKey:
			completes++
			if !r.IfNoneMatch {
				t.Fatal("multipart completion must be conditional")
			}
		}
	}
	if creates == 0 || completes == 0 || parts < 2 {
		t.Fatalf("expected multipart upload, got creates=%d parts=%d completes=%d", creates, parts, completes)
	}
	backupID := backupIDOf(t, client, f, prefix)
	head, err := client.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String(f.bucket),
		Key:    aws.String(fmt.Sprintf("%s/%s/repositories/big.tar.gz", prefix, backupID)),
	})
	if err != nil || head.ContentLength == nil || *head.ContentLength <= 64<<20 {
		t.Fatalf("archive must exceed the part threshold, got %v (%v)", head.ContentLength, err)
	}
}

// TestS3PaginationCleanup seeds more than one listing page of junk objects
// under a completed run prefix and lets retention delete it: every object
// must be removed while the permanent claim survives.
func TestS3PaginationCleanup(t *testing.T) {
	canRunRooted(t)
	f := s3Fixture(t)
	client := fixtureClient(t, f)
	dir := t.TempDir()
	srv := startSSHGitServer(t, dir)
	bin := buildBinary(t)
	origin := seedSourceRepo(t, dir, "alpha")
	prefix := fmt.Sprintf("it-page-%d", time.Now().UnixNano())
	credentialsDir := writeFixtureCredentials(t, dir, f)
	repos := fmt.Sprintf("repositories:\n  - name: alpha\n    url: ssh://root@127.0.0.1:%d%s\n", srv.port, origin)
	base := s3RunConfig{
		dir: dir, endpoint: f.endpoint, caFile: f.caPath,
		credentials: credentialsDir, bucket: f.bucket, prefix: prefix,
		workspace: filepath.Join(dir, "workspace"), reposYAML: repos,
		maxBackups: 1, retentionOn: true,
	}
	if code, out := runBackup(t, bin, writeS3Config(t, base)); code != 0 {
		t.Fatalf("first run failed (%d):\n%s", code, out)
	}
	firstID := backupIDOf(t, client, f, prefix)
	runPrefix := fmt.Sprintf("%s/%s/", prefix, firstID)

	// 1005 junk objects: crosses the 1000-key listing page boundary.
	ctx := context.Background()
	for i := 0; i < 1005; i++ {
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(f.bucket),
			Key:    aws.String(fmt.Sprintf("%sjunk/%04d", runPrefix, i)),
			Body:   strings.NewReader("x"),
		})
		if err != nil {
			t.Fatalf("seed junk %d: %v", i, err)
		}
	}

	time.Sleep(1100 * time.Millisecond)
	if code, out := runBackup(t, bin, writeS3Config(t, base)); code != 0 {
		t.Fatalf("second run failed (%d):\n%s", code, out)
	}
	if keys := listAll(t, client, f.bucket, runPrefix); len(keys) != 0 {
		t.Fatalf("paginated deletion incomplete: %d objects remain under %s", len(keys), runPrefix)
	}
	if err := headOK(t, client, f.bucket,
		fmt.Sprintf("%s/.control/claims/%s.json", prefix, firstID)); err != nil {
		t.Fatalf("permanent claim must survive deletion: %v", err)
	}
}

// TestS3SIGTERMDuringMultipart delays part uploads so the interruption
// point is deterministic: once the first part request is observed, the
// process is terminated. It must exit 143, leave no marker, and abort its
// in-flight multipart upload.
func TestS3SIGTERMDuringMultipart(t *testing.T) {
	canRunRooted(t)
	f := s3Fixture(t)
	client := fixtureClient(t, f)
	proxy, proxyEndpoint := startRecordingProxy(t, f)
	proxy.delayPartUploads.Store(int64(2 * time.Second))

	dir := t.TempDir()
	srv := startSSHGitServer(t, dir)
	bin := buildBinary(t)
	origin := seedIncompressibleRepo(t, dir, "big", 80)
	prefix := fmt.Sprintf("it-term-%d", time.Now().UnixNano())
	credentialsDir := writeFixtureCredentials(t, dir, f)
	repos := fmt.Sprintf("repositories:\n  - name: big\n    url: ssh://root@127.0.0.1:%d%s\n", srv.port, origin)
	cfg := writeS3Config(t, s3RunConfig{
		dir: dir, endpoint: proxyEndpoint, caFile: proxy.caFile,
		credentials: credentialsDir, bucket: f.bucket, prefix: prefix,
		workspace: filepath.Join(dir, "workspace"), reposYAML: repos,
		maxBackups: 5, retentionOn: true,
	})

	cmd := exec.Command(bin, "run", "--config", cfg)
	logFile, err := os.Create(filepath.Join(dir, "run.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Wait until a part upload is actually in flight, then terminate.
	deadline := time.Now().Add(120 * time.Second)
	signaled := false
	for time.Now().Before(deadline) {
		for _, r := range proxy.snapshot() {
			if r.Method == "PUT" && strings.Contains(r.Query, "uploadId=") {
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				signaled = true
				break
			}
		}
		if signaled {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !signaled {
		_ = cmd.Process.Kill()
		t.Fatal("no part upload observed before deadline")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("process did not exit after SIGTERM")
	}
	if code := cmd.ProcessState.ExitCode(); code != 143 {
		t.Fatalf("expected exit code 143, got %d", code)
	}
	waitForNoMultipartUploads(t, client, f, prefix)
	if keys := listAll(t, client, f.bucket, prefix+"/"); len(keys) != 1 {
		// Only the permanent claim may remain.
		t.Fatalf("expected only the claim object, got %v", keys)
	}
}

// listMultipartUploads returns in-progress upload keys under prefix.
func listMultipartUploads(t *testing.T, client *s3.Client, f s3FixtureInfo, prefix string) []string {
	t.Helper()
	out, err := client.ListMultipartUploads(context.Background(), &s3.ListMultipartUploadsInput{
		Bucket: aws.String(f.bucket), Prefix: aws.String(prefix + "/"),
	})
	if err != nil {
		t.Fatalf("list multipart uploads: %v", err)
	}
	var keys []string
	for _, u := range out.Uploads {
		if u.Key != nil {
			keys = append(keys, *u.Key)
		}
	}
	return keys
}

// waitForNoMultipartUploads gives the S3 service a short window to finish
// requests that were already in flight when the process received SIGTERM.
func waitForNoMultipartUploads(t *testing.T, client *s3.Client, f s3FixtureInfo, prefix string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		uploads := listMultipartUploads(t, client, f, prefix)
		if len(uploads) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("in-flight multipart uploads must be aborted, got %v", uploads)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
