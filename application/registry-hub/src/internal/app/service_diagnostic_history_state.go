package app

import (
	"context"
	"sort"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
)

func mergeDispositionRanges(current, next []diagnosticEventDisposition, events []diagnostics.Event) []diagnosticEventDisposition {
	byLast := map[string]diagnosticEventDisposition{}
	for _, value := range append(append([]diagnosticEventDisposition(nil), current...), next...) {
		byLast[value.LastEventID] = value
	}
	result := make([]diagnosticEventDisposition, 0, len(byLast))
	for _, value := range byLast {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		return eventIndex(events, result[i].FirstEventID) < eventIndex(events, result[j].FirstEventID)
	})
	return result
}

func appendDisposition(values []diagnosticEventDisposition, next diagnosticEventDisposition) []diagnosticEventDisposition {
	if len(values) > 0 && values[len(values)-1].LastEventID == next.LastEventID {
		values[len(values)-1] = next
		return values
	}
	return append(values, next)
}

func appendUniqueString(values []string, next string) []string {
	if next == "" {
		return values
	}
	for _, value := range values {
		if value == next {
			return values
		}
	}
	return append(values, next)
}

func appendUniqueStrings(values, additions []string) []string {
	for _, value := range additions {
		values = appendUniqueString(values, value)
	}
	return values
}

func eventIndex(events []diagnostics.Event, id string) int {
	for index, event := range events {
		if event.ID == id {
			return index
		}
	}
	return -1
}

func (s *Service) recordDiagnosticOutboxGap(ctx context.Context, source, version string, events []diagnostics.Event, reason string) (string, error) {
	first, last := "", ""
	if len(events) > 0 {
		first, last = events[0].ID, events[len(events)-1].ID
	}
	id := diagnostics.ID("outbox-gap", source, version, first, last, reason)
	record := diagnosticCoverageGapRecord{ID: id, SourceKey: source, SourceVersion: version, FirstEventID: first, LastEventID: last, EventCount: len(events), Reason: reason, OccurredAt: time.Now().UTC()}
	created := false
	if err := inventory.Update(ctx, s.store, diagnostics.Prefix+"gaps/"+id+".json", func(value *diagnosticCoverageGapRecord) error {
		created = false
		if value.ID == "" {
			*value = record
			created = true
		}
		return nil
	}); err != nil {
		return "", err
	}
	if !created {
		return id, nil
	}
	if err := inventory.Update(ctx, s.store, diagnostics.Prefix+"coverage.json", func(value *diagnosticHistoryCoverage) error {
		value.LastGapAt, value.Reason = record.OccurredAt, reason
		value.GapCount++
		value.DroppedEvents += uint64(len(events))
		value.DroppedBatches++
		return nil
	}); err != nil {
		return "", err
	}
	return id, nil
}

func diagnosticCriticalEvent(event diagnostics.Event) bool {
	if event.AttemptLayer == "process_observation" {
		return false
	}
	switch event.Result {
	case "failed", "done", "completed", "recovered", "solidified", "anchored", "verified_local":
		return true
	default:
		return false
	}
}
