package app

import (
	"registry-hub/internal/inventory"
	"time"
)

const (
	historyCleanupPolicyVersion   = "history-cleanup-v1"
	historyCleanupMaxPreviews     = 100
	historyCleanupMaxResults      = 20
	historyCleanupMaxScanned      = 100000
	historyCleanupBatchSize       = 32
	historyCleanupRangeBatchSize  = 8
	historyCleanupStorageRequests = 96
	historyCleanupRangeBudget     = 5 * time.Second
	historyCleanupMaxRange        = 7 * 24 * time.Hour
	historyCleanupSegmentItems    = 10000
	historyCleanupSegmentBytes    = int64(64 << 20)
	historyCleanupWorkKind        = "history_cleanup"
	historyCleanupPreviewKind     = "history_cleanup_preview"
)

var historyCleanupCategories = []string{
	"diagnostic_events",
	"terminal_work_history",
	"terminal_transfer_history",
	"storage_snapshot_history",
	"full_work_receipts",
	"diagnostic_collection_receipts",
	"transfer_observations",
}

type historyCleanupCounts struct {
	Scanned          int64            `json:"scanned"`
	Eligible         int64            `json:"eligible"`
	EligibleBytes    int64            `json:"eligibleBytes"`
	OldestEligibleAt *time.Time       `json:"oldestEligibleAt,omitempty"`
	Protected        int64            `json:"protected"`
	ProtectedBytes   int64            `json:"protectedBytes"`
	Deleted          int64            `json:"deleted"`
	DeletedBytes     int64            `json:"deletedBytes"`
	Missing          int64            `json:"missing"`
	Unconfirmed      int64            `json:"unconfirmed"`
	Skipped          int64            `json:"skipped"`
	Failed           int64            `json:"failed"`
	Oversized        int64            `json:"oversized"`
	OversizedBytes   int64            `json:"oversizedBytes"`
	SkipReasons      map[string]int64 `json:"skipReasons,omitempty"`
	Coverage         string           `json:"coverage"`
}

type historyCleanupPreviewRequest struct {
	Categories []string  `json:"categories"`
	Cutoff     time.Time `json:"cutoff"`
}

type historyCleanupRangeRequest struct {
	Category string    `json:"category"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Cursor   string    `json:"cursor,omitempty"`
}

type historyCleanupRange struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

type historyCleanupCandidateRef struct {
	Key         string                      `json:"key"`
	Version     string                      `json:"version"`
	Fingerprint string                      `json:"fingerprint,omitempty"`
	ModTime     int64                       `json:"modTime"`
	Size        int64                       `json:"size"`
	At          time.Time                   `json:"at,omitempty"`
	Index       *historyCleanupCandidateRef `json:"index,omitempty"`
	Manifest    *historyCleanupCandidateRef `json:"manifest,omitempty"`
}

type historyCleanupSegment struct {
	ID        string `json:"id"`
	PreviewID string `json:"previewID"`
	Category  string `json:"category"`
	Items     int    `json:"items"`
	Objects   int    `json:"objects"`
	Bytes     int64  `json:"bytes"`
	Chunks    int    `json:"chunks"`
}

type historyCleanupSegmentChunk struct {
	SegmentID  string                         `json:"segmentID"`
	Ordinal    int                            `json:"ordinal"`
	Candidates []historyCleanupCandidateRef   `json:"candidates"`
	Resume     *historyCleanupRangeCheckpoint `json:"resume,omitempty"`
}

type historyCleanupRangeCheckpoint struct {
	PrefixIndex    int                        `json:"prefixIndex"`
	Cursor         inventory.Cursor           `json:"cursor"`
	Scanned        int64                      `json:"scanned"`
	Counts         historyCleanupCounts       `json:"counts"`
	ChunkIndex     int                        `json:"chunkIndex"`
	SegmentID      string                     `json:"segmentID"`
	SegmentItems   int                        `json:"segmentItems"`
	SegmentObjects int                        `json:"segmentObjects"`
	SegmentBytes   int64                      `json:"segmentBytes"`
	SnapshotID     string                     `json:"snapshotID,omitempty"`
	Snapshot       historyCleanupSnapshotScan `json:"snapshot,omitempty"`
	FinishedAt     time.Time                  `json:"finishedAt,omitempty"`
}

type historyCleanupRangeContinuation struct {
	SchemaVersion   int                         `json:"schemaVersion"`
	SourcePreviewID string                      `json:"sourcePreviewID"`
	Principal       string                      `json:"principal"`
	Category        string                      `json:"category"`
	RequestedRange  historyCleanupRange         `json:"requestedRange"`
	EffectiveRange  historyCleanupRange         `json:"effectiveRange"`
	PolicyVersion   string                      `json:"policyVersion"`
	Progress        historyCleanupRangeProgress `json:"progress"`
	ExpiresAt       time.Time                   `json:"expiresAt"`
	NextPreviewID   string                      `json:"nextPreviewID,omitempty"`
}

type historyCleanupRangePreview struct {
	SchemaVersion   int                       `json:"schemaVersion"`
	ID              string                    `json:"id"`
	Principal       string                    `json:"principal"`
	Category        string                    `json:"category"`
	RequestedRange  historyCleanupRange       `json:"requestedRange"`
	EffectiveRange  historyCleanupRange       `json:"effectiveRange"`
	PolicyVersion   string                    `json:"policyVersion"`
	Status          string                    `json:"status"`
	Counts          historyCleanupCounts      `json:"counts"`
	SegmentID       string                    `json:"segmentID,omitempty"`
	SegmentItems    int                       `json:"segmentItems,omitempty"`
	SegmentBytes    int64                     `json:"segmentBytes,omitempty"`
	NextCursor      string                    `json:"nextCursor,omitempty"`
	BackfillPending []string                  `json:"backfillPending,omitempty"`
	LastError       string                    `json:"lastError,omitempty"`
	NextRetry       time.Time                 `json:"nextRetry,omitempty"`
	CreatedAt       time.Time                 `json:"createdAt"`
	CompletedAt     time.Time                 `json:"completedAt,omitempty"`
	ExpiresAt       time.Time                 `json:"expiresAt"`
	StatusURL       string                    `json:"statusURL"`
	Limits          historyCleanupRangeLimits `json:"limits"`
}

type historyCleanupRangeLimits struct {
	MaxRangeSeconds   int64 `json:"maxRangeSeconds"`
	MaxSegmentObjects int   `json:"maxSegmentObjects"`
	MaxSegmentBytes   int64 `json:"maxSegmentBytes"`
	CheckpointObjects int   `json:"checkpointObjects"`
	StorageRequests   int   `json:"storageRequests"`
	MaxStepSeconds    int64 `json:"maxStepSeconds"`
}

type historyCleanupRangeJobPayload struct {
	PreviewID     string `json:"previewID"`
	Principal     string `json:"principal"`
	PolicyVersion string `json:"policyVersion"`
}

type historyCleanupRangeProgress struct {
	PrefixIndex    int                          `json:"prefixIndex"`
	Cursor         inventory.Cursor             `json:"cursor"`
	Scanned        int64                        `json:"scanned"`
	Counts         historyCleanupCounts         `json:"counts"`
	ChunkIndex     int                          `json:"chunkIndex"`
	Chunk          []historyCleanupCandidateRef `json:"chunk,omitempty"`
	SegmentID      string                       `json:"segmentID"`
	SegmentItems   int                          `json:"segmentItems"`
	SegmentObjects int                          `json:"segmentObjects"`
	SegmentBytes   int64                        `json:"segmentBytes"`
	SnapshotID     string                       `json:"snapshotID,omitempty"`
	Snapshot       historyCleanupSnapshotScan   `json:"snapshot,omitempty"`
	FinishedAt     time.Time                    `json:"finishedAt,omitempty"`
}

type historyCleanupSnapshotScan struct {
	Manifest *historyCleanupCandidateRef `json:"manifest,omitempty"`
	Reason   string                      `json:"reason,omitempty"`
	Outside  bool                        `json:"outside,omitempty"`
	Pending  bool                        `json:"pending,omitempty"`
}
