package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

const (
	terminalWorkHistoryOrderPrefix     = diagnostics.Prefix + "work-history-order/"
	terminalTransferHistoryOrderPrefix = diagnostics.Prefix + "transfer-history-order/"
	historyProjectionIndexMarker       = "{}"
)

type historyProjectionBudget struct {
	SchemaVersion int              `json:"schemaVersion"`
	TotalBytes    int64            `json:"totalBytes"`
	Sizes         map[string]int64 `json:"sizes"`
}

func (s *Service) reserveHistoryProjection(ctx context.Context, prefix, key string, nextBytes int64, maxRecords int, maxBytes int64) (int64, error) {
	budgetKey := diagnostics.Prefix + "history-budgets/" + inventory.ID(prefix) + ".json"
	if err := s.initializeHistoryProjectionBudget(ctx, prefix, budgetKey, maxRecords, maxBytes); err != nil {
		return 0, err
	}
	var previous int64
	var full bool
	err := inventory.Update(ctx, s.store, budgetKey, func(budget *historyProjectionBudget) error {
		if budget.SchemaVersion != 1 || budget.Sizes == nil {
			return storage.ErrCorruptObject
		}
		previous, _ = budget.Sizes[key]
		if _, exists := budget.Sizes[key]; !exists && len(budget.Sizes) >= maxRecords || budget.TotalBytes-previous+nextBytes > maxBytes {
			full = true
			return nil
		}
		budget.TotalBytes += nextBytes - previous
		budget.Sizes[key] = nextBytes
		return nil
	})
	if err != nil {
		return 0, err
	}
	if full {
		return 0, diagnostics.ErrHistoryCapacity
	}
	return previous, nil
}

func (s *Service) initializeHistoryProjectionBudget(ctx context.Context, prefix, key string, maxRecords int, maxBytes int64) error {
	if _, _, err := inventory.Read[historyProjectionBudget](ctx, s.store, key); err == nil {
		return nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	entries := make([]storage.Entry, 0, 256)
	token := ""
	for {
		page, next, err := storage.Scan(ctx, s.store, prefix, token, 1000)
		if err != nil {
			return err
		}
		entries = append(entries, page...)
		if len(entries) > maxRecords {
			return diagnostics.ErrHistoryCapacity
		}
		if next == "" {
			break
		}
		token = next
	}
	budget := historyProjectionBudget{SchemaVersion: 1, Sizes: make(map[string]int64, len(entries))}
	for _, entry := range entries {
		budget.Sizes[entry.Key] = entry.Size
		budget.TotalBytes += entry.Size
	}
	if budget.TotalBytes > maxBytes {
		return diagnostics.ErrHistoryCapacity
	}
	data, err := json.Marshal(budget)
	if err != nil {
		return err
	}
	if err := inventory.ValidateWriteGuard(ctx); err != nil {
		return err
	}
	_, err = s.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err == nil {
		return nil
	}
	if !errors.Is(err, storage.ErrAlreadyExists) && !errors.Is(err, storage.ErrConditionFailed) && !errors.Is(err, storage.ErrUnknownCommit) {
		return err
	}
	if _, _, readErr := inventory.Read[historyProjectionBudget](ctx, s.store, key); readErr == nil {
		return nil
	} else {
		return errors.Join(err, readErr)
	}
}

func (s *Service) restoreHistoryProjectionBudget(ctx context.Context, prefix, key string, expected, previous int64) error {
	budgetKey := diagnostics.Prefix + "history-budgets/" + inventory.ID(prefix) + ".json"
	return inventory.Update(ctx, s.store, budgetKey, func(budget *historyProjectionBudget) error {
		current, exists := budget.Sizes[key]
		if !exists || current != expected {
			return nil
		}
		budget.TotalBytes += previous - current
		if previous == 0 {
			delete(budget.Sizes, key)
		} else {
			budget.Sizes[key] = previous
		}
		return nil
	})
}

func (s *Service) releaseHistoryProjectionBudget(ctx context.Context, prefix, key string, size int64) error {
	return s.restoreHistoryProjectionBudget(ctx, prefix, key, size, 0)
}

func historyOrderKey(prefix string, at time.Time, kind, id, revision string) string {
	orderedTime := uint64(at.UTC().UnixNano()) ^ (uint64(1) << 63)
	return fmt.Sprintf("%s%020d/%s/%s/%s.json", prefix, ^orderedTime, url.PathEscape(kind), url.PathEscape(id), url.PathEscape(revision))
}

func parseHistoryOrderKey(prefix, key string) (string, string, bool) {
	if !strings.HasPrefix(key, prefix) {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(key, prefix), "/")
	if len(parts) != 4 || len(parts[0]) != 20 || !strings.HasSuffix(parts[3], ".json") {
		return "", "", false
	}
	if _, err := strconv.ParseUint(parts[0], 10, 64); err != nil {
		return "", "", false
	}
	kind, kindErr := url.PathUnescape(parts[1])
	id, idErr := url.PathUnescape(parts[2])
	revision, revisionErr := url.PathUnescape(strings.TrimSuffix(parts[3], ".json"))
	if kindErr != nil || idErr != nil || revisionErr != nil || kind == "" || id == "" || revision == "" {
		return "", "", false
	}
	return kind, id, true
}

func historyOrderTime(prefix, key string) (time.Time, bool) {
	if !strings.HasPrefix(key, prefix) {
		return time.Time{}, false
	}
	component := strings.SplitN(strings.TrimPrefix(key, prefix), "/", 2)[0]
	ordered, err := strconv.ParseUint(component, 10, 64)
	if err != nil || len(component) != 20 {
		return time.Time{}, false
	}
	nanos := int64((^ordered) ^ (uint64(1) << 63))
	return time.Unix(0, nanos).UTC(), true
}

func putHistoryOrderIndex(ctx context.Context, store storage.Store, key string) error {
	body := []byte(historyProjectionIndexMarker)
	if err := inventory.ValidateWriteGuard(ctx); err != nil {
		return err
	}
	_, err := store.Put(ctx, key, bytes.NewReader(body), int64(len(body)), storage.PutOptions{IfAbsent: true})
	if err == nil {
		return nil
	}
	if !errors.Is(err, storage.ErrAlreadyExists) && !errors.Is(err, storage.ErrConditionFailed) && !errors.Is(err, storage.ErrUnknownCommit) {
		return err
	}
	if _, headErr := store.Head(ctx, key); headErr == nil {
		return nil
	} else if errors.Is(headErr, storage.ErrNotFound) {
		return errors.Join(err, headErr)
	} else {
		return errors.Join(err, headErr)
	}
}

func deleteHistoryOrderIndex(ctx context.Context, store storage.Store, key string) error {
	entry, err := store.Head(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := inventory.ValidateWriteGuard(ctx); err != nil {
		return err
	}
	err = store.DeleteIfMatch(ctx, key, entry.Version)
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrConditionFailed) {
		return nil
	}
	return err
}
