package s3

import (
	"path"

	"git-repo-backup/internal/manifest"
)

// Key layout inside the bucket:
//
//	<prefix>/<backup-id>/...                  final logical backup objects
//	<prefix>/.control/claims/<backup-id>.json exclusive ID reservation
//
// Claims are permanent small tombstones kept after backup deletion; they are
// not part of any backup and never count toward maxBackups.
const controlSegment = ".control"

func claimsPrefix(prefix string) string {
	return path.Join(prefix, controlSegment, "claims")
}

func claimKey(prefix, backupID string) string {
	return claimsPrefix(prefix) + "/" + backupID + ".json"
}

func runPrefix(prefix, backupID string) string {
	return path.Join(prefix, backupID) + "/"
}

func runObjectKey(prefix, backupID, name string) string {
	return path.Join(prefix, backupID, name)
}

func markerKey(prefix, backupID string) string {
	return runObjectKey(prefix, backupID, manifest.MarkerName)
}

func manifestKey(prefix, backupID string) string {
	return runObjectKey(prefix, backupID, manifest.ManifestName)
}

func archiveObjectKey(prefix, backupID, archivePath string) string {
	return path.Join(prefix, backupID, archivePath)
}
