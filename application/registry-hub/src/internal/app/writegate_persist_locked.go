package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/storage"
)

func (g *writeGate) persistLocked(ctx context.Context) error {
	g.state.SchemaVersion = gateSchemaCurrent
	g.state.ProtectionSessionCount = len(g.state.SessionIDs)
	if g.state.ProtectedCacheReservations == nil {
		g.state.ProtectedCacheReservations = make(map[string]int64)
	}
	g.state.ProtectedCacheReservationCount = len(g.state.ProtectedCacheReservations)
	data, err := json.Marshal(g.state)
	if err != nil {
		return err
	}
	options := storage.PutOptions{IfAbsent: g.version == ""}
	if g.version != "" {
		options.IfMatch = g.version
	}
	entry, err := g.store.Put(ctx, "system/gc/state.json", bytes.NewReader(data), int64(len(data)), options)
	if errors.Is(err, storage.ErrUnknownCommit) {
		if object, readErr := g.store.Get(ctx, "system/gc/state.json"); readErr == nil {
			actual, readErr := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
			_ = object.Body.Close()
			if readErr == nil && bytes.Equal(data, actual) {
				g.version = object.Version
				return nil
			}
		}
	}
	if err != nil {
		return err
	}
	g.version = entry.Version
	return nil
}

func (g *writeGate) signalLocked() {
	close(g.changed)
	g.changed = make(chan struct{})
}
