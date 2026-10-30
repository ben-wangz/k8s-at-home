package app

import (
	"context"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

func (s *Service) advanceStorageUsageMerge(ctx context.Context, state *storageCollection) error {
	m := &state.UsageMerge
	if m.Phase == "" {
		m.Phase, m.Runs = "reduce", state.UsageRuns
	}
	if m.Runs == 0 {
		m.Phase = "done"
		return nil
	}
	if m.Phase == "seed_sort" {
		return s.seedStorageUsageSort(ctx, state)
	}
	if m.Runs == 1 && m.Phase == "reduce" {
		m.Phase = "seed_sort"
		return nil
	}
	if m.Runs == 1 && m.Phase == "sort" {
		state.Snapshot.UsageRun = storageUsageRun{Stage: "sort", Level: m.Level}
		meta, _, err := inventory.Read[storageUsageRunMeta](ctx, s.store, usageRunPrefix(state.Snapshot.SnapshotID, state.Snapshot.UsageRun)+"manifest.json")
		if err != nil {
			return err
		}
		state.Snapshot.UsagePages, m.Phase = meta.Pages, "done"
		return nil
	}
	if m.Phase != "reduce" && m.Phase != "sort" {
		return storage.ErrCorruptObject
	}
	return s.mergeStorageUsagePage(ctx, state)
}

func (s *Service) seedStorageUsageSort(ctx context.Context, state *storageCollection) error {
	m := &state.UsageMerge
	run := storageUsageRun{Stage: "reduce", Level: m.Level}
	meta, _, err := inventory.Read[storageUsageRunMeta](ctx, s.store, usageRunPrefix(state.Snapshot.SnapshotID, run)+"manifest.json")
	if err != nil {
		return err
	}
	if m.SeedPage == meta.Pages {
		m.Phase, m.Level, m.Runs = "sort", 0, m.Pair
		m.Pair, m.Left = 0, storageUsagePosition{}
		return nil
	}
	rows, _, err := inventory.Read[[]storageUsage](ctx, s.store, usageRunPage(state.Snapshot.SnapshotID, run, m.SeedPage))
	if err != nil {
		return err
	}
	// A single input run may contain more than one final output page.
	start := m.Left.Offset
	end := min(start+32, len(rows))
	batch := append([]storageUsage(nil), rows[start:end]...)
	sortStorageUsage(batch, true)
	if err := s.writeUsageRun(ctx, state.Snapshot.SnapshotID, storageUsageRun{Stage: "sort", Index: m.Pair}, batch); err != nil {
		return err
	}
	m.Pair++
	m.Left.Offset = end
	if end == len(rows) {
		m.SeedPage++
		m.Left.Offset = 0
	}
	if m.SeedPage == meta.Pages {
		m.Phase, m.Level, m.Runs = "sort", 0, m.Pair
		m.Pair, m.Left = 0, storageUsagePosition{}
	}
	return nil
}

func (s *Service) mergeStorageUsagePage(ctx context.Context, state *storageCollection) error {
	m := &state.UsageMerge
	left, err := s.usageReader(ctx, state.Snapshot.SnapshotID, storageUsageRun{Stage: m.Phase, Level: m.Level, Index: m.Pair * 2}, &m.Left, true)
	if err != nil {
		return err
	}
	right, err := s.usageReader(ctx, state.Snapshot.SnapshotID, storageUsageRun{Stage: m.Phase, Level: m.Level, Index: m.Pair*2 + 1}, &m.Right, m.Pair*2+1 < m.Runs)
	if err != nil {
		return err
	}
	rows := []storageUsage{}
	done := false
	for unit := 0; unit < 32; unit++ {
		a, hasA, errA := left.peek(ctx)
		b, hasB, errB := right.peek(ctx)
		if errA != nil {
			return errA
		}
		if errB != nil {
			return errB
		}
		if !hasA && !hasB {
			done = true
			break
		}
		order := compareStorageUsage(a, b, m.Phase == "sort")
		switch {
		case hasA && hasB && order == 0 && m.Phase == "reduce":
			rows = append(rows, combineStorageUsage(a, b))
			m.Left.Offset++
			m.Right.Offset++
		case hasA && (!hasB || order <= 0):
			rows = append(rows, a)
			m.Left.Offset++
		default:
			rows = append(rows, b)
			m.Right.Offset++
		}
	}
	out := storageUsageRun{Stage: m.Phase, Level: m.Level + 1, Index: m.Pair}
	if len(rows) > 0 {
		if err := s.inventory.Immutable(ctx, usageRunPage(state.Snapshot.SnapshotID, out, m.Output.Pages), rows); err != nil {
			return err
		}
		m.Output.Pages++
		m.Output.Rows += len(rows)
	}
	if done {
		if err := s.inventory.Immutable(ctx, usageRunPrefix(state.Snapshot.SnapshotID, out)+"manifest.json", m.Output); err != nil {
			return err
		}
		m.Pair++
		m.Left, m.Right, m.Output = storageUsagePosition{}, storageUsagePosition{}, storageUsageRunMeta{}
		if m.Pair*2 >= m.Runs {
			m.Runs, m.Pair, m.Level = (m.Runs+1)/2, 0, m.Level+1
		}
	}
	return nil
}
