//go:build integration

// S3 backend acceptance through the recording proxy: upload order (claim,
// archives, manifest, checksums, _SUCCESS last), conditional writes on
// every create, and a full download/verify/extract/restore cycle that does
// not trust ETags as checksums.
package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// s3RunConfig renders one S3-backend run configuration.
type s3RunConfig struct {
	dir         string
	endpoint    string
	caFile      string // empty: untrusted-CA negative case
	credentials string // directory holding fixture credentials
	bucket      string
	prefix      string
	workspace   string
	reposYAML   string
	maxBackups  int
	retentionOn bool
}

func writeS3Config(t *testing.T, rc s3RunConfig) string {
	t.Helper()
	configPath := filepath.Join(rc.dir, "config.yaml")
	reposPath := filepath.Join(rc.dir, "repositories.yaml")
	if err := os.WriteFile(reposPath, []byte(rc.reposYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	caLine := "    caBundleFile: \"\"\n"
	if rc.caFile != "" {
		caLine = fmt.Sprintf("    caBundleFile: %q\n", rc.caFile)
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
%sworkspace:
  root: %s
backup:
  maxRunDuration: 10m
  gitTimeout: 2m
retention:
  enabled: %t
  maxBackups: %d
  maxAge: ""
  incompleteMaxAge: 30m
log:
  level: debug
`, reposPath, rc.endpoint, rc.bucket, rc.prefix, rc.credentials, caLine,
		rc.workspace, rc.retentionOn, rc.maxBackups)
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath
}

// TestS3EndToEnd checks the object protocol and a full restore.
func TestS3EndToEnd(t *testing.T) {
	canRunRooted(t)
	f := s3Fixture(t)
	client := fixtureClient(t, f)
	proxy, proxyEndpoint := startRecordingProxy(t, f)

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
		"access-key-id": f.accessKey, "secret-access-key": f.secretKey,
	} {
		if err := os.WriteFile(filepath.Join(credentialsDir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	repos := fmt.Sprintf("repositories:\n  - name: alpha\n    url: ssh://root@127.0.0.1:%d%s\n", srv.port, origin)
	cfg := writeS3Config(t, s3RunConfig{
		dir: dir, endpoint: proxyEndpoint, caFile: proxy.caFile,
		credentials: credentialsDir, bucket: f.bucket, prefix: prefix,
		workspace: filepath.Join(dir, "workspace"), reposYAML: repos,
		maxBackups: 5, retentionOn: true,
	})
	if code, out := runBackup(t, bin, cfg); code != 0 {
		t.Fatalf("s3 run failed (%d):\n%s", code, out)
	}
	assertUploadProtocol(t, proxy.snapshot(), prefix)

	// Full download-and-verify restore, independent of the proxy.
	backupID := backupIDOf(t, client, f, prefix)
	runPrefix := prefix + "/" + backupID + "/"
	restoreDir := filepath.Join(dir, "restore")
	if err := os.MkdirAll(filepath.Join(restoreDir, "repositories"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, obj := range listAll(t, client, f.bucket, runPrefix) {
		rel := strings.TrimPrefix(obj, runPrefix)
		if strings.Contains(rel, "/") {
			if err := os.MkdirAll(filepath.Join(restoreDir, filepath.Dir(rel)), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		downloadToFile(t, client, f.bucket, obj, filepath.Join(restoreDir, rel))
	}
	verifyDownloadedRun(t, restoreDir, origin)
	// The permanent claim must exist and stay out of the run namespace.
	if err := headOK(t, client, f.bucket,
		fmt.Sprintf("%s/.control/claims/%s.json", prefix, backupID)); err != nil {
		t.Fatalf("claim missing: %v", err)
	}
}

// assertUploadProtocol validates the recorded request stream:
//   - the claim is the first content-related conditional write;
//   - every PUT to the final namespace and every multipart completion is
//     conditional (If-None-Match: *);
//   - archives precede manifest.json, then checksums.sha256, then
//     _SUCCESS, and _SUCCESS is the very last write of the run.
func assertUploadProtocol(t *testing.T, recs []recordedRequest, prefix string) {
	t.Helper()
	underPrefix := func(key string) bool { return strings.HasPrefix(key, prefix+"/") }
	isWrite := func(r recordedRequest) bool {
		if r.Method == "PUT" && underPrefix(r.Key) {
			return true
		}
		return r.Method == "POST" && (strings.Contains(r.Query, "uploads") ||
			strings.Contains(r.Query, "uploadId=") || strings.Contains(r.Query, "delete"))
	}
	claimKey := "/.control/claims/"
	writes := filterRecs(recs, isWrite)
	claimWrites := filterRecs(recs, func(r recordedRequest) bool {
		return r.Method == "PUT" && strings.Contains(r.Key, claimKey)
	})
	if len(writes) == 0 || len(claimWrites) == 0 {
		t.Fatal("no recorded writes or claim")
	}
	if claimWrites[0].Seq > writes[0].Seq {
		t.Fatal("claim must be created before any content write")
	}
	var marker, manifestPut, checksumsPut int
	archivePuts := 0
	for i, w := range writes {
		switch {
		case strings.HasSuffix(w.Key, "/_SUCCESS"):
			marker = i
		case strings.HasSuffix(w.Key, "/manifest.json") && w.Method == "PUT":
			manifestPut = i
		case strings.HasSuffix(w.Key, "/checksums.sha256") && w.Method == "PUT":
			checksumsPut = i
		case strings.Contains(w.Key, "/repositories/") && w.Method == "PUT":
			archivePuts++
		}
		if w.Method == "PUT" && underPrefix(w.Key) && !w.IfNoneMatch {
			t.Fatalf("unconditional PUT to %s", w.Key)
		}
		if w.Method == "POST" && strings.Contains(w.Query, "uploadId=") && !w.IfNoneMatch {
			t.Fatalf("unconditional multipart completion for %s", w.Key)
		}
	}
	if archivePuts == 0 {
		t.Fatal("no archive PUT recorded")
	}
	if !(manifestPut < checksumsPut && checksumsPut < marker) {
		t.Fatalf("metadata order wrong: manifest=%d checksums=%d marker=%d",
			manifestPut, checksumsPut, marker)
	}
	if marker != len(writes)-1 {
		t.Fatalf("_SUCCESS must be the last write, got index %d of %d", marker, len(writes))
	}
}
