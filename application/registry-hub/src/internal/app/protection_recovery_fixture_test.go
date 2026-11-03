package app

import (
	"context"
	"io"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"sync"
	"testing"
)

type recoveryFaultStore struct {
	storage.Store
	mu         sync.Mutex
	getHook    func(string) error
	putHook    func(string, []byte) error
	deleteHook func(string) error
	reads      map[string]int
}

func (s *recoveryFaultStore) Get(ctx context.Context, key string) (storage.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reads == nil {
		s.reads = map[string]int{}
	}
	s.reads[key]++
	if s.getHook != nil {
		if err := s.getHook(key); err != nil {
			return storage.Object{}, err
		}
	}
	return s.Store.Get(ctx, key)
}

func (s *recoveryFaultStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.Entry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.putHook != nil {
		if err := s.putHook(key, data); err != nil {
			return storage.Entry{}, err
		}
	}
	return s.Store.Put(ctx, key, strings.NewReader(string(data)), size, options)
}

func (s *recoveryFaultStore) DeleteIfMatch(ctx context.Context, key, version string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteHook != nil {
		if err := s.deleteHook(key); err != nil {
			return err
		}
	}
	return s.Store.DeleteIfMatch(ctx, key, version)
}

func newRecoveryFixture(t *testing.T) (*stagedAcceptanceFixture, *recoveryFaultStore) {
	t.Helper()
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	faults := &recoveryFaultStore{Store: base}
	controlled := &recoveryStore{Store: faults}
	f := newStagedAcceptanceFixtureWithStore(t, 1<<20, controlled)
	f.service.gate.recovery = controlled
	f.service.instanceID = requestID()
	return f, faults
}

func recoveryCheckpointForTest(t *testing.T, s *Service) protectionRecoveryCheckpoint {
	t.Helper()
	c, _, err := inventory.Read[protectionRecoveryCheckpoint](context.Background(), s.store, protectionRecoveryKey)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
