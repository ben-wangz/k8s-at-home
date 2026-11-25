package app

import (
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"time"
)

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
	SegmentID string `json:"segmentID,omitempty"`
}

type historyCleanupJobPayload struct {
	PreviewID     string `json:"previewID"`
	Principal     string `json:"principal"`
	PolicyVersion string `json:"policyVersion"`
	SegmentID     string `json:"segmentID,omitempty"`
}

type historyCleanupProgress struct {
	CategoryIndex          int                                `json:"categoryIndex"`
	PrefixIndex            int                                `json:"prefixIndex"`
	Cursor                 inventory.Cursor                   `json:"cursor"`
	EventWatermarkAdvanced bool                               `json:"eventWatermarkAdvanced,omitempty"`
	EventSealedThrough     time.Time                          `json:"eventSealedThrough,omitzero"`
	EventSealTo            time.Time                          `json:"eventSealTo,omitzero"`
	EventWatermark         diagnostics.EventWatermarkProgress `json:"eventWatermark,omitzero"`
	SegmentID              string                             `json:"segmentID,omitempty"`
	SegmentOffset          int                                `json:"segmentOffset,omitempty"`
	SegmentStarted         bool                               `json:"segmentStarted,omitempty"`
	SegmentPrimarySettled  bool                               `json:"segmentPrimarySettled,omitempty"`
	SegmentIndexStarted    bool                               `json:"segmentIndexStarted,omitempty"`
	EventSealedDate        string                             `json:"eventSealedDate,omitempty"`
	Counts                 map[string]historyCleanupCounts    `json:"counts"`
}

type historyCleanupOperation struct {
	SchemaVersion          int                                `json:"schemaVersion"`
	ID                     string                             `json:"id"`
	PreviewID              string                             `json:"previewID"`
	Principal              string                             `json:"principal"`
	Categories             []string                           `json:"categories"`
	Cutoff                 time.Time                          `json:"cutoff"`
	Range                  historyCleanupRange                `json:"range,omitempty"`
	SegmentID              string                             `json:"segmentID,omitempty"`
	SegmentOffset          int                                `json:"segmentOffset,omitempty"`
	SegmentStarted         bool                               `json:"segmentStarted,omitempty"`
	SegmentPrimarySettled  bool                               `json:"segmentPrimarySettled,omitempty"`
	SegmentIndexStarted    bool                               `json:"segmentIndexStarted,omitempty"`
	EventWatermarkAdvanced bool                               `json:"eventWatermarkAdvanced,omitempty"`
	EventSealedThrough     time.Time                          `json:"eventSealedThrough,omitzero"`
	EventSealTo            time.Time                          `json:"eventSealTo,omitzero"`
	EventWatermark         diagnostics.EventWatermarkProgress `json:"eventWatermark,omitzero"`
	PolicyVersion          string                             `json:"policyVersion"`
	Status                 string                             `json:"status"`
	Counts                 map[string]historyCleanupCounts    `json:"counts"`
	QueueSource            string                             `json:"queueSource"`
	CreatedAt              time.Time                          `json:"createdAt"`
	CompletedAt            time.Time                          `json:"completedAt,omitempty"`
	ExpiresAt              time.Time                          `json:"expiresAt"`
	IdempotencyID          string                             `json:"idempotencyID,omitempty"`
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
	Range         historyCleanupRange             `json:"range,omitempty"`
	SegmentID     string                          `json:"segmentID,omitempty"`
	Counts        map[string]historyCleanupCounts `json:"counts"`
	Scanned       int64                           `json:"scanned"`
	Deleted       int64                           `json:"deleted"`
	Missing       int64                           `json:"missing"`
	Unconfirmed   int64                           `json:"unconfirmed"`
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
	PolicyVersion       string                                 `json:"policyVersion"`
	Policies            map[string]string                      `json:"policies"`
	DurationsSeconds    map[string]int64                       `json:"durationsSeconds"`
	LastCleanup         *historyCleanupTaskView                `json:"lastCleanup,omitempty"`
	KnownEligibleVolume map[string]historyCleanupCounts        `json:"knownEligibleVolume"`
	Workers             map[string]historyCleanupWorkerSummary `json:"workers"`
	RangeLimits         historyCleanupRangeLimits              `json:"rangeLimits"`
	ReceiptBackfill     historyCleanupReceiptBackfill          `json:"receiptBackfill"`
	ObservedAt          time.Time                              `json:"observedAt"`
	NextRun             time.Time                              `json:"nextRun,omitempty"`
}

type historyCleanupWorkerSummary struct {
	LastSuccessAt       time.Time `json:"lastSuccessAt,omitempty"`
	LastSuccessfulRound uint64    `json:"lastSuccessfulRound"`
	PendingRetries      int64     `json:"pendingRetries"`
	Deleted             int64     `json:"deleted"`
}

type historyCleanupReceiptBackfill struct {
	InitialRoundVerified    bool  `json:"initialRoundVerified"`
	AutomaticCleanupEnabled bool  `json:"automaticCleanupEnabled"`
	PendingRetries          int64 `json:"pendingRetries"`
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
