package app

import (
	"time"

	"registry-hub/internal/inventory"
)

const (
	historyCleanupPolicyVersion = "history-cleanup-v1"
	historyCleanupMaxPreviews   = 100
	historyCleanupMaxResults    = 20
	historyCleanupMaxScanned    = 100000
	historyCleanupBatchSize     = 32
	historyCleanupWorkKind      = "history_cleanup"
)

var historyCleanupCategories = []string{
	"diagnostic_events",
	"terminal_work_history",
	"terminal_transfer_history",
	"storage_snapshot_history",
}

type historyCleanupCounts struct {
	Scanned        int64            `json:"scanned"`
	Eligible       int64            `json:"eligible"`
	EligibleBytes  int64            `json:"eligibleBytes"`
	Protected      int64            `json:"protected"`
	ProtectedBytes int64            `json:"protectedBytes"`
	Deleted        int64            `json:"deleted"`
	DeletedBytes   int64            `json:"deletedBytes"`
	Missing        int64            `json:"missing"`
	Skipped        int64            `json:"skipped"`
	Failed         int64            `json:"failed"`
	SkipReasons    map[string]int64 `json:"skipReasons,omitempty"`
	Coverage       string           `json:"coverage"`
}

type historyCleanupPreviewRequest struct {
	Categories []string  `json:"categories"`
	Cutoff     time.Time `json:"cutoff"`
}

type historyCleanupPreview struct {
	SchemaVersion int                             `json:"schemaVersion"`
	ID            string                          `json:"id"`
	Principal     string                          `json:"principal"`
	Categories    []string                        `json:"categories"`
	Cutoff        time.Time                       `json:"cutoff"`
	EventBefore   string                          `json:"eventBefore,omitempty"`
	PolicyVersion string                          `json:"policyVersion"`
	Counts        map[string]historyCleanupCounts `json:"counts"`
	Complete      bool                            `json:"complete"`
	Coverage      string                          `json:"coverage"`
	CreatedAt     time.Time                       `json:"createdAt"`
	ExpiresAt     time.Time                       `json:"expiresAt"`
}

type historyCleanupExecutionRequest struct {
	PreviewID string `json:"previewID"`
}

type historyCleanupJobPayload struct {
	PreviewID     string `json:"previewID"`
	Principal     string `json:"principal"`
	PolicyVersion string `json:"policyVersion"`
}

type historyCleanupProgress struct {
	CategoryIndex          int                             `json:"categoryIndex"`
	PrefixIndex            int                             `json:"prefixIndex"`
	Cursor                 inventory.Cursor                `json:"cursor"`
	EventWatermarkAdvanced bool                            `json:"eventWatermarkAdvanced,omitempty"`
	Counts                 map[string]historyCleanupCounts `json:"counts"`
}

type historyCleanupOperation struct {
	SchemaVersion int                             `json:"schemaVersion"`
	ID            string                          `json:"id"`
	PreviewID     string                          `json:"previewID"`
	Principal     string                          `json:"principal"`
	Categories    []string                        `json:"categories"`
	Cutoff        time.Time                       `json:"cutoff"`
	PolicyVersion string                          `json:"policyVersion"`
	Status        string                          `json:"status"`
	Counts        map[string]historyCleanupCounts `json:"counts"`
	QueueSource   string                          `json:"queueSource"`
	CreatedAt     time.Time                       `json:"createdAt"`
	CompletedAt   time.Time                       `json:"completedAt,omitempty"`
	ExpiresAt     time.Time                       `json:"expiresAt"`
	IdempotencyID string                          `json:"idempotencyID,omitempty"`
}

type historyCleanupIdempotency struct {
	SchemaVersion int       `json:"schemaVersion"`
	RequestID     string    `json:"requestID"`
	OperationID   string    `json:"operationID"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

type historyCleanupTaskView struct {
	ID            string                          `json:"id"`
	PreviewID     string                          `json:"previewID"`
	Status        string                          `json:"status"`
	StatusURL     string                          `json:"statusURL"`
	PolicyVersion string                          `json:"policyVersion"`
	Categories    []string                        `json:"categories"`
	Cutoff        time.Time                       `json:"cutoff"`
	Counts        map[string]historyCleanupCounts `json:"counts"`
	Scanned       int64                           `json:"scanned"`
	Deleted       int64                           `json:"deleted"`
	Missing       int64                           `json:"missing"`
	Skipped       int64                           `json:"skipped"`
	Failed        int64                           `json:"failed"`
	DeletedBytes  int64                           `json:"deletedBytes"`
	LastError     string                          `json:"lastError,omitempty"`
	NextRetry     time.Time                       `json:"nextRetry,omitempty"`
	CreatedAt     time.Time                       `json:"createdAt"`
	CompletedAt   time.Time                       `json:"completedAt,omitempty"`
	ExpiresAt     time.Time                       `json:"expiresAt,omitempty"`
}

type historyCleanupRetentionView struct {
	PolicyVersion       string                          `json:"policyVersion"`
	Policies            map[string]string               `json:"policies"`
	DurationsSeconds    map[string]int64                `json:"durationsSeconds"`
	LastCleanup         *historyCleanupTaskView         `json:"lastCleanup,omitempty"`
	KnownEligibleVolume map[string]historyCleanupCounts `json:"knownEligibleVolume"`
	ObservedAt          time.Time                       `json:"observedAt"`
	NextRun             time.Time                       `json:"nextRun,omitempty"`
}

type historyCleanupCandidate struct {
	Eligible bool
	Reason   string
}

type historyCleanupEntryProgress struct {
	Deleted      int64
	DeletedBytes int64
	Missing      int64
	Skipped      int64
}
