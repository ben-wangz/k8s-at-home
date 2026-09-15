package storage

import (
	"fmt"
	"regexp"
	"time"
)

// BackupIDPattern is the canonical UTC backup ID grammar.
var BackupIDPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z$`)

// FormatBackupID renders a UTC timestamp as a backup ID.
func FormatBackupID(t time.Time) string {
	return t.UTC().Format("20060102T150405Z")
}

// ParseBackupID validates that id is a syntactically and calendrically valid
// UTC backup ID and returns the timestamp it encodes.
func ParseBackupID(id string) (time.Time, error) {
	if !BackupIDPattern.MatchString(id) {
		return time.Time{}, fmt.Errorf("invalid backup id format")
	}
	t, err := time.Parse("20060102T150405Z", id)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid backup id date")
	}
	if FormatBackupID(t) != id {
		return time.Time{}, fmt.Errorf("invalid backup id date")
	}
	return t, nil
}
