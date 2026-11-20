package app

import (
	"context"
	"registry-hub/internal/inventory"
	"strings"
	"time"
)

func (s *Service) advanceStorageLogicalUsage(ctx context.Context, state *storageCollection) error {
	if state.UsagePending != nil {
		return s.publishStorageUsagePending(ctx, state)
	}
	before := *state
	working := *state
	working.AttributionUsageCursor = cloneStorageUsageCursor(state.AttributionUsageCursor)
	working.Usage = cloneStorageUsage(state.Usage)
	working.Snapshot.Reasons = append([]string(nil), state.Snapshot.Reasons...)
	started := time.Now()
	for unit := 0; unit < 16 && working.AttributionPhase == 2; unit++ {
		if unit > 0 && (time.Since(started) > 2*time.Second || !storageQuantumTime(ctx, time.Second)) {
			break
		}
		found, err := s.storageLogicalUsageEntry(ctx, &working)
		if err != nil {
			return err
		}
		if !found {
			break
		}
	}
	if state.Snapshot.UsageVersion > 0 && len(working.Usage) > 0 {
		pending, err := s.prepareStorageUsagePending(ctx, before, working)
		if err != nil {
			return err
		}
		state.UsagePending = pending
		state.Snapshot.AttributionCoverage = working.Snapshot.AttributionCoverage
		state.Snapshot.Reasons = working.Snapshot.Reasons
		return nil
	}
	state.AttributionUsageCursor = working.AttributionUsageCursor
	state.AttributionPhase = working.AttributionPhase
	state.Usage = working.Usage
	state.Snapshot.AttributionCoverage = working.Snapshot.AttributionCoverage
	state.Snapshot.Reasons = working.Snapshot.Reasons
	return nil
}

func (s *Service) storageLogicalUsageEntry(ctx context.Context, state *storageCollection) (bool, error) {
	prefix := storageInventoryPrefix + "snapshots/" + state.Snapshot.SnapshotID + "/associations/"
	entry, found, err := state.AttributionUsageCursor.Peek(ctx, s.store, prefix, 32)
	if err != nil {
		return false, err
	}
	if !found {
		if state.AttributionUsageCursor.Done {
			state.AttributionPhase = 3
		}
		return false, nil
	}
	set, _, err := inventory.Read[storageAssociationSet](ctx, s.store, entry.Key)
	if err != nil {
		return false, err
	}
	if set.Truncated {
		state.Snapshot.AttributionCoverage = "partial; captured inventory associations truncated"
		state.Snapshot.Reasons = appendUniqueReason(state.Snapshot.Reasons, "association_limit")
	}
	domain, _, _ := strings.Cut(strings.TrimPrefix(entry.Key, prefix), "/")
	object := storageObject{Domain: domain, Associations: set.Items}
	for key, group := range storageUsageGroups(object) {
		usage, exists := state.Usage[key]
		if !exists && state.Snapshot.UsageVersion == 0 && len(state.Usage) >= 4096 {
			state.Snapshot.Reasons = appendUniqueReason(state.Snapshot.Reasons, "usage_group_limit")
			continue
		}
		if !exists {
			usage = group
		}
		usage.Attribution = "logical object union in captured inventory; physical group unions may overlap"
		if set.DeclaredBytes < 0 || set.Truncated {
			usage.LogicalUnknown = true
		}
		if usage.LogicalUnknown {
			usage.LogicalBytes = nil
		} else {
			bytes := set.DeclaredBytes
			if usage.LogicalBytes != nil {
				bytes += *usage.LogicalBytes
			}
			usage.LogicalBytes = &bytes
		}
		state.Usage[key] = usage
	}
	state.AttributionUsageCursor.Advance()
	return true, nil
}
