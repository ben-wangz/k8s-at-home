package app

import (
	"context"
	"registry-hub/internal/storage"
	"slices"
)

type storageCollectionCost struct {
	InputObjects       int64 `json:"inputObjects"`
	InputBytes         int64 `json:"inputBytes"`
	SummaryOnlyObjects int64 `json:"summaryOnlyObjects"`
	OutputObjects      int64 `json:"outputObjects"`
	OutputBytes        int64 `json:"outputBytes"`
}

func storageSummaryOnly(state *storageCollection) bool {
	return slices.Contains(state.Snapshot.Domains["state"].SummaryOnlyCategories, "storage_inventory_output")
}

func storageSummaryOnlyCategory(state *storageCollection, category string) bool {
	return slices.Contains(state.Snapshot.Domains["state"].SummaryOnlyCategories, category)
}

func (s *Service) collectStorageOutputCost(ctx context.Context, state *storageCollection) error {
	prefix := storageInventoryPrefix + "snapshots/" + state.Snapshot.SnapshotID + "/"
	entries, next, err := storage.Scan(ctx, s.store, prefix, state.OutputCostToken, 32)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		// A retried manifest write may already exist after an unknown commit.
		if entry.Key != prefix+"manifest.json" {
			state.Snapshot.Cost.OutputObjects++
			state.Snapshot.Cost.OutputBytes += entry.Size
		}
	}
	state.OutputCostToken, state.OutputCostComplete = next, next == ""
	return nil
}

func applyStorageOutputSummary(state *storageCollection) {
	usages := append([]storageUsage(nil), state.PendingSummaries...)
	if state.PendingSummary.Objects > 0 {
		usages = append(usages, state.PendingSummary)
	}
	for _, usage := range usages {
		if usage.Objects == 0 {
			continue
		}
		state.Snapshot.Cost.SummaryOnlyObjects += usage.Objects
		domain := state.Snapshot.Domains[usage.Domain]
		if domain.Categories == nil {
			domain.Categories = map[string]storageUsage{}
		}
		bytes, objects := usage.ObjectBytes, usage.Objects
		if domain.ObjectBytes != nil {
			bytes += *domain.ObjectBytes
			objects += *domain.Objects
		}
		domain.ObjectBytes, domain.Objects = &bytes, &objects
		category := combineStorageUsage(domain.Categories[usage.Category], usage)
		category.Domain, category.Dimension, category.Name = usage.Domain, usage.Dimension, usage.Name
		domain.Categories[usage.Category] = category
		state.Snapshot.Domains[usage.Domain] = domain
		if usage.Category == "storage_inventory_output" {
			if state.OutputUsage.Objects == 0 {
				state.OutputUsage = usage
			} else {
				state.OutputUsage = combineStorageUsage(state.OutputUsage, usage)
			}
			continue
		}
		key := storageUsageKey(usage)
		state.Usage[key] = combineStorageUsage(state.Usage[key], usage)
	}
	state.PendingSummary = storageUsage{}
	state.PendingSummaries = nil
}
