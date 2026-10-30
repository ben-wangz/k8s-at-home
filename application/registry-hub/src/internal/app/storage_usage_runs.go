package app

import (
	"context"
	"fmt"
	"registry-hub/internal/inventory"
	"slices"
	"strings"
)

type storageUsageRun struct {
	Stage string `json:"stage"`
	Level int    `json:"level"`
	Index int    `json:"index"`
}

type storageUsageRunMeta struct {
	Pages int `json:"pages"`
	Rows  int `json:"rows"`
}

type storageUsagePosition struct {
	Page   int `json:"page"`
	Offset int `json:"offset"`
}

type storageUsageMerge struct {
	Phase    string               `json:"phase"`
	Level    int                  `json:"level"`
	Runs     int                  `json:"runs"`
	Pair     int                  `json:"pair"`
	Left     storageUsagePosition `json:"left"`
	Right    storageUsagePosition `json:"right"`
	Output   storageUsageRunMeta  `json:"output"`
	SeedPage int                  `json:"seedPage"`
}

func usageRunPrefix(snapshot string, run storageUsageRun) string {
	return fmt.Sprintf("%ssnapshots/%s/usage/%s/%04d/%08d/", storageInventoryPrefix, snapshot, run.Stage, run.Level, run.Index)
}

func usageRunPage(snapshot string, run storageUsageRun, page int) string {
	return fmt.Sprintf("%s%08d.json", usageRunPrefix(snapshot, run), page)
}

func compareStorageUsage(a, b storageUsage, ranked bool) int {
	if ranked && a.ObjectBytes != b.ObjectBytes {
		if a.ObjectBytes > b.ObjectBytes {
			return -1
		}
		return 1
	}
	return strings.Compare(storageUsageKey(a), storageUsageKey(b))
}

func sortStorageUsage(rows []storageUsage, ranked bool) {
	slices.SortFunc(rows, func(a, b storageUsage) int { return compareStorageUsage(a, b, ranked) })
}

func (s *Service) flushStorageUsage(ctx context.Context, state *storageCollection) error {
	if len(state.Usage) == 0 {
		return nil
	}
	rows := make([]storageUsage, 0, len(state.Usage))
	for _, usage := range state.Usage {
		rows = append(rows, usage)
	}
	sortStorageUsage(rows, false)
	run := storageUsageRun{Stage: "reduce", Index: state.UsageRuns}
	if err := s.writeUsageRun(ctx, state.Snapshot.SnapshotID, run, rows); err != nil {
		return err
	}
	state.UsageRuns++
	state.Usage = map[string]storageUsage{}
	return nil
}

func (s *Service) writeUsageRun(ctx context.Context, snapshot string, run storageUsageRun, rows []storageUsage) error {
	if err := s.inventory.Immutable(ctx, usageRunPage(snapshot, run, 0), rows); err != nil {
		return err
	}
	return s.inventory.Immutable(ctx, usageRunPrefix(snapshot, run)+"manifest.json", storageUsageRunMeta{Pages: 1, Rows: len(rows)})
}

func combineStorageUsage(a, b storageUsage) storageUsage {
	a.ObjectBytes += b.ObjectBytes
	a.Objects += b.Objects
	a.UnknownAttributionBytes += b.UnknownAttributionBytes
	a.ResidentPayloadBytes = addUsageBytes(a.ResidentPayloadBytes, b.ResidentPayloadBytes)
	a.ExclusiveBytes = addUsageBytes(a.ExclusiveBytes, b.ExclusiveBytes)
	a.SharedBytes = addUsageBytes(a.SharedBytes, b.SharedBytes)
	a.LogicalUnknown = a.LogicalUnknown || b.LogicalUnknown
	a.LogicalBytes = addUsageBytes(a.LogicalBytes, b.LogicalBytes)
	if a.LogicalUnknown {
		a.LogicalBytes = nil
	}
	if a.Attribution == "" {
		a.Attribution = b.Attribution
	}
	return a
}

func addUsageBytes(a, b *int64) *int64 {
	if a == nil && b == nil {
		return nil
	}
	value := int64(0)
	if a != nil {
		value += *a
	}
	if b != nil {
		value += *b
	}
	return &value
}

type usageRunReader struct {
	service    *Service
	snapshot   string
	run        storageUsageRun
	position   *storageUsagePosition
	meta       storageUsageRunMeta
	rows       []storageUsage
	loadedPage int
}

func (s *Service) usageReader(ctx context.Context, snapshot string, run storageUsageRun, position *storageUsagePosition, exists bool) (*usageRunReader, error) {
	reader := &usageRunReader{service: s, snapshot: snapshot, run: run, position: position, loadedPage: -1}
	if exists {
		meta, _, err := inventory.Read[storageUsageRunMeta](ctx, s.store, usageRunPrefix(snapshot, run)+"manifest.json")
		if err != nil {
			return nil, err
		}
		reader.meta = meta
	}
	return reader, nil
}

func (r *usageRunReader) peek(ctx context.Context) (storageUsage, bool, error) {
	for r.position.Page < r.meta.Pages {
		if r.loadedPage != r.position.Page {
			rows, _, err := inventory.Read[[]storageUsage](ctx, r.service.store, usageRunPage(r.snapshot, r.run, r.position.Page))
			if err != nil {
				return storageUsage{}, false, err
			}
			r.rows, r.loadedPage = rows, r.position.Page
		}
		if r.position.Offset < len(r.rows) {
			return r.rows[r.position.Offset], true, nil
		}
		r.position.Page++
		r.position.Offset = 0
	}
	return storageUsage{}, false, nil
}
