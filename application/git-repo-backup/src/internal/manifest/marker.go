package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// SuccessMarker is the small JSON commit marker uploaded or created last.
// It carries nothing that is only knowable after it is written.
type SuccessMarker struct {
	SchemaVersion   int    `json:"schemaVersion"`
	BackupID        string `json:"backupId"`
	ManifestSHA256  string `json:"manifestSHA256"`
	ChecksumsSHA256 string `json:"checksumsSHA256"`
}

// BuildMarker derives the commit marker from the finalized metadata bytes.
func BuildMarker(backupID string, manifestBytes, checksumsBytes []byte) SuccessMarker {
	mDigest := sha256.Sum256(manifestBytes)
	cDigest := sha256.Sum256(checksumsBytes)
	return SuccessMarker{
		SchemaVersion:   SchemaVersion,
		BackupID:        backupID,
		ManifestSHA256:  hex.EncodeToString(mDigest[:]),
		ChecksumsSHA256: hex.EncodeToString(cDigest[:]),
	}
}

// Encode serializes the marker deterministically.
func (m SuccessMarker) Encode() ([]byte, error) {
	return json.Marshal(m)
}

// ParseMarker decodes and sanity-checks marker bytes read back from storage.
func ParseMarker(data []byte) (SuccessMarker, error) {
	var m SuccessMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return SuccessMarker{}, fmt.Errorf("invalid success marker")
	}
	if m.SchemaVersion != SchemaVersion {
		return SuccessMarker{}, fmt.Errorf("unsupported marker schemaVersion")
	}
	if m.BackupID == "" {
		return SuccessMarker{}, fmt.Errorf("marker missing backupId")
	}
	if err := validateHex(m.ManifestSHA256); err != nil {
		return SuccessMarker{}, fmt.Errorf("marker manifestSHA256: %w", err)
	}
	if err := validateHex(m.ChecksumsSHA256); err != nil {
		return SuccessMarker{}, fmt.Errorf("marker checksumsSHA256: %w", err)
	}
	return m, nil
}

// Matches compares the parsed marker against locally known metadata bytes.
func (m SuccessMarker) Matches(backupID string, manifestBytes, checksumsBytes []byte) bool {
	want := BuildMarker(backupID, manifestBytes, checksumsBytes)
	return m == want
}
