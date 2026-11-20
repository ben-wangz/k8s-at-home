package diagnostics

import "time"

const (
	diagnosticsS3SecretSuffix     = "-external-backend-s3"
	diagnosticsS3ObjectPrefix     = "registry-hub-backend/diagnostics/v1/storage/"
	diagnosticsStorageKeyPrefix   = "diagnostics/v1/storage/"
	diagnosticsS3Maintenance      = "registry-hub-backend/background/v1/maintenance-checkpoints/storage_inventory.json"
	diagnosticsS3MaintenanceLease = "registry-hub-backend/leases/background/maintenance/storage_inventory"
	diagnosticsS3CollectorLease   = "registry-hub-backend/leases/diagnostics/storage-inventory"
)

type seededUsageRow struct {
	Mask                    uint8  `json:"groupMask,omitempty"`
	Category                string `json:"category,omitempty"`
	Repository              string `json:"repository,omitempty"`
	ImageID                 string `json:"imageID,omitempty"`
	Platform                string `json:"platform,omitempty"`
	Domain                  string `json:"domain"`
	Dimension               string `json:"dimension"`
	Name                    string `json:"name"`
	ObjectBytes             int64  `json:"objectBytes"`
	Objects                 int64  `json:"objects"`
	LogicalBytes            *int64 `json:"logicalBytes"`
	ResidentPayloadBytes    *int64 `json:"residentPayloadBytes"`
	LogicalUnknown          bool   `json:"logicalUnknown"`
	ExclusiveBytes          *int64 `json:"exclusiveBytes"`
	SharedBytes             *int64 `json:"sharedBytes"`
	Attribution             string `json:"attribution"`
	UnknownAttributionBytes int64  `json:"unknownAttributionBytes"`
}

type seededUsageRun struct {
	Stage string `json:"stage"`
	Level int    `json:"level"`
	Index int    `json:"index"`
}

type seededUsageManifest struct {
	Pages int `json:"pages"`
	Rows  int `json:"rows"`
}

type seededCursor struct {
	Done bool `json:"done,omitempty"`
}

type seededPendingBatch struct {
	Version        int                 `json:"version"`
	BatchID        string              `json:"batchID"`
	SnapshotID     string              `json:"snapshotID"`
	Run            seededUsageRun      `json:"run"`
	StartCursor    seededCursor        `json:"startCursor"`
	NextCursor     seededCursor        `json:"nextCursor"`
	NextPhase      int                 `json:"nextPhase"`
	PayloadKey     string              `json:"payloadKey"`
	PayloadSHA256  string              `json:"payloadSHA256"`
	PageKey        string              `json:"pageKey"`
	PageSHA256     string              `json:"pageSHA256"`
	ManifestKey    string              `json:"manifestKey"`
	Manifest       seededUsageManifest `json:"manifest"`
	ManifestSHA256 string              `json:"manifestSHA256"`
	PreparedAt     time.Time           `json:"preparedAt"`
}

type seededStorageSnapshot struct {
	SnapshotID   string                    `json:"snapshotID"`
	StartedAt    time.Time                 `json:"startedAt"`
	UsageVersion int                       `json:"usageVersion"`
	Domains      map[string]map[string]any `json:"domains"`
}

type seededStorageCollection struct {
	Snapshot               seededStorageSnapshot     `json:"snapshot"`
	Domain                 int                       `json:"domain"`
	UsageRuns              int                       `json:"usageRuns"`
	Usage                  map[string]seededUsageRow `json:"usage"`
	AttributionPhase       int                       `json:"attributionPhase"`
	AttributionUsageCursor seededCursor              `json:"attributionUsageCursor"`
	UsagePending           *seededPendingBatch       `json:"usagePending,omitempty"`
	AutomaticRestarts      int                       `json:"automaticRestarts,omitempty"`
	RecoveredFromBuildID   string                    `json:"recoveredFromBuildID,omitempty"`
}

type seededMaintenanceCheckpoint struct {
	Next time.Time `json:"next"`
}

type seededLeaseRecord struct {
	State     string    `json:"state"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type scanStatusEvidence struct {
	StartedAt            time.Time        `json:"startedAt"`
	ObservedAt           time.Time        `json:"observedAt"`
	ProgressAt           time.Time        `json:"progressAt"`
	LastScanCompletedAt  time.Time        `json:"lastScanCompletedAt"`
	LastSuccessAt        time.Time        `json:"lastSuccessAt"`
	LastEntityProgressAt time.Time        `json:"lastEntityProgressAt"`
	LastCursorProgressAt time.Time        `json:"lastCursorProgressAt"`
	LastHeartbeatAt      time.Time        `json:"lastHeartbeatAt"`
	LastSuccessfulRound  uint64           `json:"lastSuccessfulRound"`
	OldestDueRetryAt     time.Time        `json:"oldestDueRetryAt,omitempty"`
	NextRetryAt          time.Time        `json:"nextRetryAt,omitempty"`
	NextAttemptAt        time.Time        `json:"nextAttemptAt,omitempty"`
	DispatchLagSeconds   float64          `json:"dispatchLagSeconds"`
	PendingRetries       int64            `json:"pendingRetries"`
	Operations           uint64           `json:"operations"`
	Processed            uint64           `json:"processed"`
	Counts               map[string]int64 `json:"counts,omitempty"`
	CheckpointAt         time.Time        `json:"checkpointAt,omitempty"`
	Complete             bool             `json:"complete"`
}

type scanCheckpointEvidence struct {
	Status scanStatusEvidence `json:"status"`
}
