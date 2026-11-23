package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

const diagnosticPendingBatchPrefix = "diagnostics/v2/event-batch-pending/"

type diagnosticPendingSource struct {
	SourceKey       string `json:"sourceKey"`
	SourceVersion   string `json:"sourceVersion,omitempty"`
	PreviousEventID string `json:"previousEventID,omitempty"`
	FirstEventID    string `json:"firstEventID"`
	LastEventID     string `json:"lastEventID"`
	EventCount      int    `json:"eventCount"`
	ContentSHA256   string `json:"contentSHA256"`
}

type diagnosticPendingBatch struct {
	Date             string                    `json:"date"`
	Critical         bool                      `json:"critical"`
	FirstAdmittedAt  time.Time                 `json:"firstAdmittedAt"`
	OrdinaryDeadline time.Time                 `json:"ordinaryDeadline,omitempty"`
	CriticalDeadline time.Time                 `json:"criticalDeadline,omitempty"`
	FlushOutcome     *diagnosticPendingOutcome `json:"flushOutcome,omitempty"`
	Events           []diagnostics.Event       `json:"events"`
	Sources          []diagnosticPendingSource `json:"sources"`
}

type diagnosticPendingOutcome struct {
	Disposition string `json:"disposition"`
	Reason      string `json:"reason,omitempty"`
	BatchID     string `json:"batchID,omitempty"`
	BatchSHA256 string `json:"batchSHA256,omitempty"`
}

type diagnosticPendingPartition struct {
	SchemaVersion int                     `json:"schemaVersion"`
	Date          string                  `json:"date"`
	Batch         *diagnosticPendingBatch `json:"batch,omitempty"`
}

func diagnosticPendingBatchKey(date string) string {
	return diagnosticPendingBatchPrefix + date + ".json"
}

func (batch diagnosticPendingBatch) deadline() time.Time {
	if batch.Critical {
		return batch.CriticalDeadline
	}
	return batch.OrdinaryDeadline
}

func diagnosticPendingBatchSize(events []diagnostics.Event) (int, error) {
	size := 256
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			return 0, err
		}
		size += len(encoded) + 1
	}
	return size, nil
}

func diagnosticPendingBatchFits(events []diagnostics.Event, date string) bool {
	if len(events) == 0 || len(events) > diagnostics.BatchMaxEvents {
		return false
	}
	critical := diagnosticCriticalEvent(events[0])
	for _, event := range events {
		if event.ID == "" || event.OccurredAt.IsZero() || diagnostics.EventBatchDate(event) != date || diagnosticCriticalEvent(event) != critical {
			return false
		}
	}
	size, err := diagnosticPendingBatchSize(events)
	return err == nil && size <= diagnostics.BatchMaxBytes
}

func diagnosticEventRangeHash(events []diagnostics.Event) (string, error) {
	encoded, err := json.Marshal(events)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}

func diagnosticPendingKeyDate(key string) (string, bool) {
	name := strings.TrimPrefix(key, diagnosticPendingBatchPrefix)
	if !strings.HasSuffix(name, ".json") {
		return "", false
	}
	date := strings.TrimSuffix(name, ".json")
	if len(date) != 8 {
		return "", false
	}
	if _, err := time.Parse("20060102", date); err != nil {
		return "", false
	}
	return date, true
}

func (s *Service) saveDiagnosticPendingPartition(ctx context.Context, date string, partition diagnosticPendingPartition) error {
	if partition.Date != date || partition.SchemaVersion != 1 || partition.Batch == nil {
		return fmt.Errorf("invalid diagnostic pending partition: %w", storage.ErrCorruptObject)
	}
	return inventory.Update(ctx, s.store, diagnosticPendingBatchKey(date), func(current *diagnosticPendingPartition) error {
		if current.SchemaVersion != 0 && (current.SchemaVersion != 1 || current.Date != date) {
			return storage.ErrCorruptObject
		}
		*current = partition
		return nil
	})
}
