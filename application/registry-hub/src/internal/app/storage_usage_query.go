package app

import (
	"context"
	"net/http"
	"net/url"
	"registry-hub/internal/inventory"
	"slices"
	"strconv"
	"strings"
)

type storageUsagePage struct {
	diagnosticPage[storageUsage]
	SnapshotID string   `json:"snapshotID"`
	Coverage   string   `json:"coverage"`
	Reasons    []string `json:"reasons"`
	Truncated  bool     `json:"truncated"`
}

func storageUsageSelection(values url.Values) (uint8, int) {
	mask, dimension := uint8(0), -1
	for bit, key := range []string{"category", "repository", "imageID", "platform"} {
		if values.Get(key) != "" {
			mask |= 1 << bit
			dimension = bit
		}
	}
	if selected := values.Get("dimension"); selected != "" {
		dimension = slices.Index(storageDimensions, selected)
	} else if mask != 0 && mask&(mask-1) != 0 {
		dimension = 0
	}
	if dimension >= 0 {
		mask |= 1 << dimension
	}
	return mask, dimension
}

func matchesStorageUsageGroup(usage storageUsage, values url.Values) bool {
	mask, _ := storageUsageSelection(values)
	if mask == 0 && usage.Mask&(usage.Mask-1) != 0 || mask != 0 && usage.Mask != mask {
		return false
	}
	if values.Get("domain") != "" && values.Get("domain") != usage.Domain {
		return false
	}
	for bit, key := range []string{"category", "repository", "imageID", "platform"} {
		filter := values.Get(key)
		name := usageName(usage, bit)
		if filter != "" && filter != name && (key != "repository" || !strings.HasSuffix(name, "/"+filter)) {
			return false
		}
	}
	return true
}

func (s *Service) serveStorageUsage(writer http.ResponseWriter, request *http.Request, ctx context.Context, snapshot storageSnapshot, cursor diagnosticCursor, limit int) {
	values := request.URL.Query()
	if snapshot.UsageVersion == 0 && !validLegacyStorageUsageQuery(values) {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	position, err := strconv.Atoi(cursor.Storage)
	if cursor.Storage == "" {
		position, err = 0, nil
	}
	if err != nil || position < 0 || cursor.Phase < 0 {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	page := storageUsagePage{diagnosticPage: diagnosticPage[storageUsage]{Items: []storageUsage{}, ObservedAt: snapshot.CompletedAt, Consistency: "immutable snapshot; objectBytes descending"}, SnapshotID: snapshot.SnapshotID, Coverage: snapshot.AttributionCoverage, Reasons: snapshot.Reasons, Truncated: slices.Contains(snapshot.Reasons, "usage_group_limit") || slices.Contains(snapshot.Reasons, "association_limit")}
	rows := snapshot.Usage
	offset := position
	if snapshot.UsageVersion > 0 {
		if position > snapshot.UsagePages {
			writeInventoryError(writer, inventory.ErrInvalid)
			return
		}
		rows, offset = nil, cursor.Phase
		if position < snapshot.UsagePages {
			rows, _, err = inventory.Read[[]storageUsage](ctx, s.store, usageRunPage(snapshot.SnapshotID, snapshot.UsageRun, position))
			if err != nil {
				writeInventoryError(writer, err)
				return
			}
		}
	}
	if offset > len(rows) {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	_, dimension := storageUsageSelection(values)
	for offset < len(rows) && page.Scanned < limit {
		usage := rows[offset]
		offset++
		page.Scanned++
		if matchesStorageUsage(usage, values) {
			usage.LogicalUnknown = usage.LogicalUnknown || usage.LogicalBytes == nil
			if dimension >= 0 && usage.Mask > 0 {
				usage.Dimension, usage.Name = storageDimensions[dimension], usageName(usage, dimension)
			}
			page.Items = append(page.Items, usage)
		}
	}
	more := offset < len(rows)
	cursor.Storage = strconv.Itoa(offset)
	if snapshot.UsageVersion > 0 {
		if !more {
			position++
			offset = 0
		}
		more = position < snapshot.UsagePages
		cursor.Storage, cursor.Phase = strconv.Itoa(position), offset
	}
	if more {
		page.NextToken = s.diagnosticToken(cursor)
	}
	writeJSON(writer, http.StatusOK, page)
}
