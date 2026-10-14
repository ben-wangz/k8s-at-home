package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
)

var (
	ErrNotFound         = errors.New("storage object not found")
	ErrAlreadyExists    = errors.New("storage object already exists")
	ErrConditionFailed  = errors.New("storage condition failed")
	ErrUnavailable      = errors.New("storage unavailable")
	ErrTimeout          = errors.New("storage operation timed out")
	ErrPermissionDenied = errors.New("storage permission denied")
	ErrCorruptObject    = errors.New("storage object is corrupt")
	ErrUnknownCommit    = errors.New("storage commit state is unknown")
	ErrLeaseLost        = errors.New("storage lease was lost")
	ErrInvalidKey       = errors.New("invalid storage key")
)

type Object struct {
	Body    io.ReadCloser
	Size    int64
	Version string
	ModTime int64
}

type PutOptions struct {
	IfAbsent bool
	IfMatch  string
}

type Entry struct {
	Key     string
	Version string
	Size    int64
	ModTime int64
}

type Store interface {
	Get(context.Context, string) (Object, error)
	Head(context.Context, string) (Entry, error)
	Put(context.Context, string, io.Reader, int64, PutOptions) (Entry, error)
	Delete(context.Context, string) error
	DeleteIfMatch(context.Context, string, string) error
	List(context.Context, string, string, int) ([]Entry, string, error)
	Close() error
}

func Probe(ctx context.Context, store Store) error {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	key := "system/probes/" + hex.EncodeToString(random[:])
	first := []byte("registry-hub-storage-probe-v1")
	second := []byte("registry-hub-storage-probe-v2")
	entry, err := store.Put(ctx, key, bytes.NewReader(first), int64(len(first)), PutOptions{IfAbsent: true})
	if err != nil {
		return err
	}
	if _, err := store.Put(ctx, key, bytes.NewReader(first), int64(len(first)), PutOptions{IfAbsent: true}); !errors.Is(err, ErrAlreadyExists) {
		_ = store.DeleteIfMatch(ctx, key, entry.Version)
		return errors.New("storage does not enforce conditional create")
	}
	updated, err := store.Put(ctx, key, bytes.NewReader(second), int64(len(second)), PutOptions{IfMatch: entry.Version})
	if err != nil {
		_ = store.DeleteIfMatch(ctx, key, entry.Version)
		return err
	}
	if err := store.DeleteIfMatch(ctx, key, entry.Version); !errors.Is(err, ErrConditionFailed) {
		_ = store.DeleteIfMatch(ctx, key, updated.Version)
		return errors.New("storage does not enforce conditional delete")
	}
	if err := store.DeleteIfMatch(ctx, key, updated.Version); err != nil {
		return err
	}
	return nil
}
