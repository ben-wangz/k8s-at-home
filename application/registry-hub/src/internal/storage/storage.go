package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"
)

var (
	ErrNotFound             = errors.New("storage object not found")
	ErrAlreadyExists        = errors.New("storage object already exists")
	ErrConditionFailed      = errors.New("storage condition failed")
	ErrUnavailable          = errors.New("storage unavailable")
	ErrTimeout              = errors.New("storage operation timed out")
	ErrPermissionDenied     = errors.New("storage permission denied")
	ErrCorruptObject        = errors.New("storage object is corrupt")
	ErrUnknownCommit        = errors.New("storage commit state is unknown")
	ErrLeaseLost            = errors.New("storage lease was lost")
	ErrInvalidKey           = errors.New("invalid storage key")
	ErrReplayDirectoryInUse = errors.New("S3 replay directory is already in use")
)

type Object struct {
	Body              io.ReadCloser
	Size              int64
	Version           string
	ModTime           int64
	ChecksumAlgorithm string
	ChecksumValue     string
	ChecksumScope     string
}

type PutOptions struct {
	IfAbsent bool
	IfMatch  string
}

type Entry struct {
	Key                 string
	Version             string
	MetadataFingerprint string
	Size                int64
	ModTime             int64
	ChecksumAlgorithm   string
	ChecksumValue       string
	ChecksumScope       string
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
	return ProbeWithCleanupContext(ctx, ctx, store)
}

func ProbeWithCleanupContext(ctx, lifecycle context.Context, store Store) error {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	key := "system/probes/" + hex.EncodeToString(random[:])
	first := []byte("registry-hub-storage-probe-v1")
	second := []byte("registry-hub-storage-probe-v2")
	entry, err := store.Put(ctx, key, bytes.NewReader(first), int64(len(first)), PutOptions{IfAbsent: true})
	if err != nil {
		return fmt.Errorf("storage probe initial create: %w", err)
	}
	if _, err := store.Put(ctx, key, bytes.NewReader(first), int64(len(first)), PutOptions{IfAbsent: true}); !errors.Is(err, ErrAlreadyExists) {
		if err != nil {
			return probeCleanup(lifecycle, store, key, entry.Version, fmt.Errorf("storage probe conditional create: %w", err))
		}
		return probeCleanup(lifecycle, store, key, entry.Version, errors.New("storage does not enforce conditional create"))
	}
	updated, err := store.Put(ctx, key, bytes.NewReader(second), int64(len(second)), PutOptions{IfMatch: entry.Version})
	if err != nil {
		return probeCleanup(lifecycle, store, key, entry.Version, fmt.Errorf("storage probe conditional update: %w", err))
	}
	if err := store.DeleteIfMatch(ctx, key, entry.Version); !errors.Is(err, ErrConditionFailed) {
		if err != nil {
			return probeCleanup(lifecycle, store, key, updated.Version, fmt.Errorf("storage probe conditional delete: %w", err))
		}
		return probeCleanup(lifecycle, store, key, updated.Version, errors.New("storage does not enforce conditional delete"))
	}
	if err := store.DeleteIfMatch(ctx, key, updated.Version); err != nil {
		return fmt.Errorf("storage probe cleanup delete: %w", err)
	}
	return nil
}

func probeCleanup(lifecycle context.Context, store Store, key, version string, cause error) error {
	ctx, cancel := context.WithTimeout(lifecycle, 5*time.Second)
	defer cancel()
	if err := store.DeleteIfMatch(ctx, key, version); err != nil {
		return errors.Join(cause, fmt.Errorf("storage probe auxiliary cleanup: %w", err))
	}
	return cause
}
