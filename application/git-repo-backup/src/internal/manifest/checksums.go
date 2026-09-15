package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// ChecksumsName and ManifestName are the fixed metadata file names.
const (
	ManifestName    = "manifest.json"
	ChecksumsName   = "checksums.sha256"
	MarkerName      = "_SUCCESS"
	RepositoriesDir = "repositories"
)

// BuildChecksums renders the GNU sha256sum-format checksum file from the
// already-finalized manifest bytes (digest of manifest.json itself) and the
// per-repository digests recorded in the manifest. The manifest bytes must
// be the exact bytes stored; they are never re-marshaled here.
func BuildChecksums(manifestBytes []byte, m *Manifest) ([]byte, error) {
	manifestDigest := sha256.Sum256(manifestBytes)
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(manifestDigest[:]), ManifestName)
	entries := make([]RepositoryEntry, len(m.Repositories))
	copy(entries, m.Repositories)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	for _, e := range entries {
		if err := validateHex(e.SHA256); err != nil {
			return nil, fmt.Errorf("repository %s: %w", e.Name, err)
		}
		fmt.Fprintf(&b, "%s  %s\n", e.SHA256, e.Archive)
	}
	return []byte(b.String()), nil
}

func validateHex(s string) error {
	if len(s) != 64 {
		return fmt.Errorf("sha256 must be 64 hex characters")
	}
	if _, err := hex.DecodeString(s); err != nil {
		return fmt.Errorf("sha256 must be lowercase hex")
	}
	return nil
}
