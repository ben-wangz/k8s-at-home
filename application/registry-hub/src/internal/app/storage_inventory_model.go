package app

import (
	"registry-hub/internal/inventory"
	"time"
)

const storageInventoryPrefix = "diagnostics/v1/storage/"

type storageObject struct {
	ID          string    `json:"id"`
	PhysicalID  string    `json:"physicalID"`
	Domain      string    `json:"domain"`
	Category    string    `json:"category"`
	Version     string    `json:"version"`
	ObjectBytes int64     `json:"objectBytes"`
	ModifiedAt  time.Time `json:"modifiedAt"`
	ObservedAt  time.Time `json:"observedAt"`
	workAssociations
	AssociationState      string               `json:"associationState"`
	Shared                *bool                `json:"shared"`
	ProtectionState       string               `json:"protectionState"`
	ReclaimState          string               `json:"reclaimState"`
	ReclaimReason         string               `json:"reclaimReason"`
	Cleaner               string               `json:"cleaner,omitempty"`
	WorkID                string               `json:"workID,omitempty"`
	CleanupState          string               `json:"cleanupState"`
	EvidenceSource        string               `json:"evidenceSource"`
	Associations          []storageAssociation `json:"associations"`
	AssociationsTruncated bool                 `json:"associationsTruncated"`
	CleanerURL            string               `json:"cleanerURL,omitempty"`
	WorkURL               string               `json:"workURL,omitempty"`
	NextAttemptAt         time.Time            `json:"nextAttemptAt"`
	LastProgressAt        time.Time            `json:"lastProgressAt"`
}

type storageUsage struct {
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

type storageDomainSummary struct {
	Domain      string                  `json:"domain"`
	State       string                  `json:"state"`
	ObjectBytes *int64                  `json:"objectBytes"`
	Objects     *int64                  `json:"objects"`
	Categories  map[string]storageUsage `json:"categories"`
	Reason      string                  `json:"reason,omitempty"`
	Coverage    []string                `json:"coverage"`
}

type storageSnapshot struct {
	SnapshotID             string                          `json:"snapshotID"`
	StartedAt              time.Time                       `json:"startedAt"`
	CompletedAt            time.Time                       `json:"completedAt"`
	ExpiresAt              time.Time                       `json:"expiresAt"`
	Pages                  int                             `json:"pages"`
	Domains                map[string]storageDomainSummary `json:"domains"`
	Usage                  []storageUsage                  `json:"usage"`
	UsageVersion           int                             `json:"usageVersion"`
	UsagePages             int                             `json:"usagePages"`
	UsageRun               storageUsageRun                 `json:"usageRun"`
	Consistency            string                          `json:"consistency"`
	DownloadReservedBytes  *int64                          `json:"downloadReservedBytes"`
	DownloadReservations   *int                            `json:"downloadReservations"`
	ReservationObservedAt  time.Time                       `json:"reservationObservedAt"`
	UnderlyingDiskBytes    *int64                          `json:"underlyingDiskBytes"`
	Reasons                []string                        `json:"reasons"`
	InventorySnapshotID    string                          `json:"inventorySnapshotID,omitempty"`
	InventoryObservedAt    time.Time                       `json:"inventoryObservedAt"`
	AttributionCoverage    string                          `json:"attributionCoverage"`
	TransferTemporaryBytes map[string]int64                `json:"transferTemporaryBytes"`
}

type storageCollection struct {
	Snapshot               storageSnapshot         `json:"snapshot"`
	Domain                 int                     `json:"domain"`
	Token                  string                  `json:"token"`
	Usage                  map[string]storageUsage `json:"usage"`
	UsageRuns              int                     `json:"usageRuns"`
	UsageMerge             storageUsageMerge       `json:"usageMerge"`
	NextScanAt             time.Time               `json:"nextScanAt"`
	PendingRows            []storageObject         `json:"pendingRows,omitempty"`
	PendingNext            string                  `json:"pendingNext,omitempty"`
	Pending                bool                    `json:"pending"`
	AttributionPhase       int                     `json:"attributionPhase"`
	AttributionCursor      inventory.Cursor        `json:"attributionCursor"`
	AttributionImage       *inventory.Image        `json:"attributionImage,omitempty"`
	AttributionPage        int                     `json:"attributionPage"`
	AttributionPages       int                     `json:"attributionPages"`
	AttributionOffset      int                     `json:"attributionOffset"`
	AttributionUsageCursor inventory.Cursor        `json:"attributionUsageCursor"`
}
