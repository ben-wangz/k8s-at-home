// Package manifest defines the schema v1 manifest, the GNU-style checksum
// file, and the _SUCCESS commit marker.
package manifest

import "time"

// SchemaVersion is the manifest schema version.
const SchemaVersion = 1

// Manifest describes one complete backup run. Repositories are sorted by
// name. No source URLs, credentials, or key material appear here.
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	BackupID      string            `json:"backupId"`
	StartedAt     time.Time         `json:"startedAt"`
	CompletedAt   time.Time         `json:"completedAt"`
	Repositories  []RepositoryEntry `json:"repositories"`
	ToolVersions  ToolVersions      `json:"toolVersions"`
}

// RepositoryEntry records one archived repository. HeadRef is empty for a
// detached or unborn HEAD; HeadOID is set for detached HEADs.
type RepositoryEntry struct {
	Name        string    `json:"name"`
	Archive     string    `json:"archive"`
	SHA256      string    `json:"sha256"`
	SizeBytes   int64     `json:"sizeBytes"`
	RefCount    int       `json:"refCount"`
	StartedAt   time.Time `json:"startedAt,omitempty"`
	CompletedAt time.Time `json:"completedAt,omitempty"`
	HeadRef     string    `json:"headRef,omitempty"`
	HeadOID     string    `json:"headOID,omitempty"`
}

// ToolVersions records the backup binary and Git versions.
type ToolVersions struct {
	Backup string `json:"backup"`
	Git    string `json:"git"`
}

// ArchivePath is the in-backup object path of a repository archive.
func ArchivePath(name string) string { return "repositories/" + name + ".tar.gz" }

// SortByKey orders repositories by name.
func (m *Manifest) SortRepositories() {
	for i := 1; i < len(m.Repositories); i++ {
		for j := i; j > 0 && m.Repositories[j-1].Name > m.Repositories[j].Name; j-- {
			m.Repositories[j-1], m.Repositories[j] = m.Repositories[j], m.Repositories[j-1]
		}
	}
}
