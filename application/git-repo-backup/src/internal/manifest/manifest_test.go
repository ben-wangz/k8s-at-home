package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func sampleManifest() (*Manifest, []byte) {
	m := &Manifest{
		SchemaVersion: 1,
		BackupID:      "20260914T020000Z",
		StartedAt:     time.Date(2026, 9, 14, 2, 0, 0, 0, time.UTC),
		CompletedAt:   time.Date(2026, 9, 14, 2, 1, 0, 0, time.UTC),
		Repositories: []RepositoryEntry{
			{Name: "zeta", Archive: ArchivePath("zeta"), SHA256: strings.Repeat("a", 64), SizeBytes: 2, RefCount: 1},
			{Name: "alpha", Archive: ArchivePath("alpha"), SHA256: strings.Repeat("b", 64), SizeBytes: 1, RefCount: 3},
		},
		ToolVersions: ToolVersions{Backup: "dev", Git: "2.43"},
	}
	data, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return m, data
}

func TestBuildChecksumsFormat(t *testing.T) {
	m, data := sampleManifest()
	out, err := BuildChecksums(data, m)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	wantDigest := sha256.Sum256(data)
	if !strings.HasPrefix(lines[0], hex.EncodeToString(wantDigest[:])+"  manifest.json") {
		t.Fatalf("manifest line wrong: %q", lines[0])
	}
	if !strings.Contains(lines[1], "  repositories/alpha.tar.gz") || !strings.Contains(lines[2], "repositories/zeta.tar.gz") {
		t.Fatalf("repositories not sorted by name: %q", lines)
	}
	for _, line := range lines {
		if !strings.Contains(line, "  ") {
			t.Fatalf("GNU two-space separator missing: %q", line)
		}
	}
}

func TestBuildChecksumsRejectsBadDigest(t *testing.T) {
	m, _ := sampleManifest()
	m.Repositories[0].SHA256 = "nothex"
	if _, err := BuildChecksums([]byte("{}"), m); err == nil {
		t.Fatal("expected digest rejection")
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	m, manifestBytes := sampleManifest()
	checksums, err := BuildChecksums(manifestBytes, m)
	if err != nil {
		t.Fatal(err)
	}
	marker := BuildMarker("20260914T020000Z", manifestBytes, checksums)
	data, err := marker.Encode()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseMarker(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed != marker {
		t.Fatalf("marker changed: %+v vs %+v", parsed, marker)
	}
	if !marker.Matches("20260914T020000Z", manifestBytes, checksums) {
		t.Fatal("marker must match its own inputs")
	}
	if marker.Matches("20260914T020001Z", manifestBytes, checksums) {
		t.Fatal("marker must not match another backup id")
	}
}

func TestParseMarkerRejects(t *testing.T) {
	badDigest := strings.Repeat("z", 64)
	for _, body := range []string{
		"",
		"{}",
		`{"schemaVersion":2,"backupId":"x","manifestSHA256":"` + strings.Repeat("a", 64) + `","checksumsSHA256":"` + strings.Repeat("b", 64) + `"}`,
		`{"schemaVersion":1,"backupId":"x","manifestSHA256":"` + badDigest + `","checksumsSHA256":"` + strings.Repeat("b", 64) + `"}`,
	} {
		if _, err := ParseMarker([]byte(body)); err == nil {
			t.Errorf("expected rejection for %q", body)
		}
	}
}
