package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"registry-hub/internal/storage"
	"time"
)

func terminalScope(ctx context.Context, key string) *terminalWriteScope {
	if !recoverySourceKey(key) {
		return nil
	}
	scope, _ := ctx.Value(terminalWriteScopeKey{}).(*terminalWriteScope)
	return scope
}

func terminalPut(ctx context.Context, scope *terminalWriteScope, store storage.Store, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if size < 0 || size > 8<<20 {
		return storage.Entry{}, storage.ErrCorruptObject
	}
	data, err := io.ReadAll(io.LimitReader(body, size+1))
	if err != nil || int64(len(data)) != size {
		return storage.Entry{}, storage.ErrCorruptObject
	}
	hash := sha256.Sum256(data)
	if err := scope.begin(ctx, terminalMutation{Operation: "put", Key: key, Version: options.IfMatch, SHA256: hex.EncodeToString(hash[:])}); err != nil {
		return storage.Entry{}, err
	}
	writeCtx, cancel := scope.writeContext(ctx)
	defer cancel()
	entry, err := store.Put(writeCtx, key, bytes.NewReader(data), size, options)
	return scope.finish(store, entry, err)
}

func terminalDelete(ctx context.Context, scope *terminalWriteScope, store storage.Store, key, version string) error {
	if err := scope.begin(ctx, terminalMutation{Operation: "delete", Key: key, Version: version}); err != nil {
		return err
	}
	var err error
	writeCtx, cancel := scope.writeContext(ctx)
	defer cancel()
	if version == "" {
		err = store.Delete(writeCtx, key)
	} else {
		err = store.DeleteIfMatch(writeCtx, key, version)
	}
	_, err = scope.finish(store, storage.Entry{}, err)
	return err
}

func (s *terminalWriteScope) writeContext(source context.Context) (context.Context, context.CancelFunc) {
	// Once journaled, one conditional metadata operation may finish across the quantum boundary.
	ctx, cancel := context.WithTimeout(s.parent, 5*time.Second)
	if identity, ok := source.Value(diagnosticContextKey{}).(backgroundDiagnosticContext); ok {
		ctx = context.WithValue(ctx, diagnosticContextKey{}, identity)
	}
	ctx = context.WithValue(ctx, storageOperationIDKey{}, s.job.ID)
	return ctx, cancel
}
