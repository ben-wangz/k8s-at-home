package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

const Retention = 7 * 24 * time.Hour
const DailyBytesLimit = 16 << 20

var ErrHistoryCapacity = errors.New("diagnostic history capacity reached")

type Log struct{ Store storage.Store }
type HistoryBudget struct {
	ReservedBytes int64  `json:"reservedBytes"`
	Rejected      uint64 `json:"rejected"`
}

func EventKey(event Event) string {
	return Prefix + "events/" + event.OccurredAt.UTC().Format("20060102/150405.000000000") + "-" + event.ID + ".json"
}

func (log Log) Append(ctx context.Context, event Event) error {
	if event.ID == "" || event.OccurredAt.IsZero() || time.Since(event.OccurredAt) > Retention {
		return nil
	}
	key := EventKey(event)
	if _, err := log.Store.Head(ctx, key); err == nil {
		return nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if len(data) > 8192 {
		return storage.ErrCorruptObject
	}
	rejected := false
	err = inventory.Update(ctx, log.Store, Prefix+"event-budgets/"+event.OccurredAt.UTC().Format("20060102")+".json", func(budget *HistoryBudget) error {
		rejected = budget.ReservedBytes+int64(len(data)) > DailyBytesLimit
		if rejected {
			budget.Rejected++
		} else {
			budget.ReservedBytes += int64(len(data))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if rejected {
		return ErrHistoryCapacity
	}
	_, err = log.Store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if errors.Is(err, storage.ErrAlreadyExists) || errors.Is(err, storage.ErrConditionFailed) {
		return nil
	}
	return err
}
