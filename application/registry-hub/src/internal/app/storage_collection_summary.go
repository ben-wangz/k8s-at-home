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
	usage := state.PendingSummary
	if usage.Objects == 0 {
		return
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
	domain.Categories[usage.Category] = combineStorageUsage(domain.Categories[usage.Category], usage)
	category := domain.Categories[usage.Category]
	category.Domain, category.Dimension, category.Name = usage.Domain, usage.Dimension, usage.Name
	domain.Categories[usage.Category] = category
	state.Snapshot.Domains[usage.Domain] = domain
	if state.OutputUsage.Objects == 0 {
		state.OutputUsage = usage
	} else {
		state.OutputUsage = combineStorageUsage(state.OutputUsage, usage)
	}
	state.PendingSummary = storageUsage{}
}
