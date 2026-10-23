package download

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
)

func TestSlowNormalEOFFlushUsesTransferContext(t *testing.T) {
	_, backing := testDownloadStore(t)
	store := &downloadFaultStore{Store: backing, slowStateOffset: 3, stateDelay: 5200 * time.Millisecond}
	manager := New(store, 1<<20)
	manager.chunkSize = 4
	content := []byte("abc")
	digest := artifact.CalculateSHA256(content)
	request := testRequest(content, digest, 41, func(context.Context, int64, int64, string) (*http.Response, context.CancelFunc, error) {
		return testResponse(http.StatusOK, content, int64(len(content)), io.NopCloser(bytes.NewReader(content))), func() {}, nil
	})
	request.FinalizeTimeout = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	started := time.Now()
	result, err := manager.Fetch(ctx, request)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed < 5*time.Second || elapsed >= 15*time.Second {
		t.Fatalf("Fetch() elapsed %s, want the delayed write to finish between 5s and 15s", elapsed)
	}
	if result.Size != int64(len(content)) {
		t.Fatalf("Fetch() size = %d, want %d", result.Size, len(content))
	}
}

func TestPersistenceFailureVetoesRetryAndCommittedStateRecovers(t *testing.T) {
	ctx, backing := testDownloadStore(t)
	store := &downloadFaultStore{Store: backing, failStateOffset: 3, failStateMode: "error"}
	manager := New(store, 1<<20)
	manager.chunkSize = 4
	content := []byte("abc")
	digest := artifact.CalculateSHA256(content)
	calls := 0
	request := testRequest(content, digest, 42, func(context.Context, int64, int64, string) (*http.Response, context.CancelFunc, error) {
		calls++
		return testResponse(http.StatusOK, content, int64(len(content)), io.NopCloser(bytes.NewReader(content))), func() {}, nil
	})
	request.Retryable = func(error) bool { return true }
	request.WaitRetry = func(context.Context, string, int) error { return errors.New("unexpected upstream wait") }

	if _, err := manager.Fetch(ctx, request); err == nil {
		t.Fatal("Fetch() succeeded after a failed state commit")
	} else {
		var persistenceErr *PersistenceError
		if !errors.As(err, &persistenceErr) || !errors.Is(err, storage.ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Fetch() error = %v, want typed storage timeout", err)
		}
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	stateData, _, err := manager.readState(ctx, stateKey(request.Identity, digest))
	if err != nil {
		t.Fatalf("read committed state: %v", err)
	}
	committed, err := decodeState(stateData, request)
	if err != nil || committed.Offset != 0 || committed.Index != "" {
		t.Fatalf("failed candidate leaked into state: offset=%d index=%q error=%v", committed.Offset, committed.Index, err)
	}

	manager = New(store, 1<<20)
	manager.chunkSize = 4
	request.OwnerFence++
	request.Source = func(context.Context, int64, int64, string) (*http.Response, context.CancelFunc, error) {
		return testResponse(http.StatusOK, content, int64(len(content)), io.NopCloser(bytes.NewReader(content))), func() {}, nil
	}
	if _, err := manager.Fetch(ctx, request); err != nil {
		t.Fatalf("independent Fetch() did not recover the durable prefix: %v", err)
	}
	body, size, err := manager.Open(ctx, request.Identity, digest, request.MaxBytes)
	if err != nil {
		t.Fatalf("Open() after recovery: %v", err)
	}
	actual, readErr := io.ReadAll(body)
	_ = body.Close()
	if readErr != nil || size != int64(len(content)) || !bytes.Equal(actual, content) {
		t.Fatalf("recovered blob=%q size=%d error=%v", actual, size, readErr)
	}
}

func TestUnknownStateCommitRequiresExactReadback(t *testing.T) {
	for _, mode := range []string{"applied", "not-applied"} {
		t.Run(mode, func(t *testing.T) {
			ctx, backing := testDownloadStore(t)
			store := &downloadFaultStore{Store: backing, failStateOffset: 3, failStateMode: mode}
			manager := New(store, 1<<20)
			manager.chunkSize = 4
			content := []byte("abc")
			digest := artifact.CalculateSHA256(content)
			calls := 0
			request := testRequest(content, digest, 43, func(context.Context, int64, int64, string) (*http.Response, context.CancelFunc, error) {
				calls++
				return testResponse(http.StatusOK, content, int64(len(content)), io.NopCloser(bytes.NewReader(content))), func() {}, nil
			})
			request.Retryable = func(error) bool { return true }
			_, err := manager.Fetch(ctx, request)
			if mode == "applied" {
				if err != nil {
					t.Fatalf("Fetch() after confirmed lost acknowledgment: %v", err)
				}
			} else {
				var persistenceErr *PersistenceError
				if !errors.As(err, &persistenceErr) || !errors.Is(err, storage.ErrUnknownCommit) {
					t.Fatalf("Fetch() error = %v, want unresolved persistence result", err)
				}
			}
			if calls != 1 {
				t.Fatalf("upstream calls = %d, want 1", calls)
			}
		})
	}
}

func TestQuotaUnknownCommitUsesReadbackWithoutRetry(t *testing.T) {
	for _, mode := range []string{"applied", "not-applied"} {
		t.Run(mode, func(t *testing.T) {
			ctx, backing := testDownloadStore(t)
			store := &downloadFaultStore{Store: backing, quotaFailureMode: mode}
			manager := New(store, 1024)
			request := Request{Identity: "registry/quota-unknown", Digest: "sha256:quota-unknown"}
			err := manager.reserve(ctx, request, "session", 50)
			if mode == "applied" {
				if err != nil {
					t.Fatalf("reserve after confirmed lost acknowledgment: %v", err)
				}
				if used, sessions, usageErr := manager.Usage(ctx); usageErr != nil || used != 50 || sessions != 1 {
					t.Fatalf("confirmed reservation usage=(%d,%d,%v)", used, sessions, usageErr)
				}
			} else {
				var persistenceErr *PersistenceError
				if !errors.As(err, &persistenceErr) || !errors.Is(err, storage.ErrUnknownCommit) {
					t.Fatalf("reserve error = %v, want unresolved commit", err)
				}
			}
			if got := store.putCount(quotaKey); got != 1 {
				t.Fatalf("quota PUT count = %d, want no same-call retry", got)
			}
		})
	}
}

func TestQuotaConflictRetriesAreBoundedAndRespectCapacity(t *testing.T) {
	t.Run("maximum attempts", func(t *testing.T) {
		ctx, backing := testDownloadStore(t)
		store := &downloadFaultStore{Store: backing, quotaConflicts: 32}
		manager := New(store, 1024)
		waits := 0
		manager.quotaWait = func(context.Context, int) error {
			waits++
			return nil
		}
		err := manager.reserve(ctx, Request{Identity: "registry/conflicts", Digest: "sha256:conflicts"}, "session", 50)
		if !errors.Is(err, storage.ErrConditionFailed) || waits != 15 || store.putCount(quotaKey) != 16 {
			t.Fatalf("conflict result=%v waits=%d puts=%d, want 16 attempts and 15 waits", err, waits, store.putCount(quotaKey))
		}
	})
	t.Run("concurrent reservations", func(t *testing.T) {
		ctx, backing := testDownloadStore(t)
		store := &downloadFaultStore{Store: backing}
		manager := New(store, 100)
		manager.quotaWait = func(context.Context, int) error { return nil }
		const workers = 8
		results := make(chan error, workers)
		var group sync.WaitGroup
		for index := 0; index < workers; index++ {
			group.Add(1)
			go func(index int) {
				defer group.Done()
				request := Request{Identity: fmt.Sprintf("registry/concurrent-%d", index), Digest: fmt.Sprintf("sha256:concurrent-%d", index)}
				results <- manager.reserve(ctx, request, fmt.Sprintf("session-%d", index), 25)
			}(index)
		}
		group.Wait()
		close(results)
		succeeded, exhausted := 0, 0
		for err := range results {
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, ErrQuotaExceeded):
				exhausted++
			default:
				t.Fatalf("concurrent reserve returned unexpected error: %v", err)
			}
		}
		used, sessions, err := manager.Usage(ctx)
		if err != nil || succeeded != 4 || exhausted != 4 || used != 100 || sessions != 4 {
			t.Fatalf("concurrent reservations succeeded=%d exhausted=%d usage=(%d,%d,%v)", succeeded, exhausted, used, sessions, err)
		}
	})
}

func TestQuotaReservationNoOpAndConflictBackoff(t *testing.T) {
	ctx, backing := testDownloadStore(t)
	store := &downloadFaultStore{Store: backing}
	manager := New(store, 1024)
	var waits []int
	manager.quotaWait = func(_ context.Context, conflict int) error {
		waits = append(waits, conflict)
		return nil
	}
	request := Request{Identity: "registry/quota", Digest: "sha256:quota"}
	if err := manager.reserve(ctx, request, "session-a", 100); err != nil {
		t.Fatalf("initial reserve: %v", err)
	}
	before := store.putCount(quotaKey)
	if err := manager.reserve(ctx, request, "session-a", 80); err != nil {
		t.Fatalf("unchanged reserve: %v", err)
	}
	if got := store.putCount(quotaKey); got != before {
		t.Fatalf("unchanged reservation PUT count = %d, want %d", got, before)
	}
	if err := manager.reserve(ctx, request, "session-a", 120); err != nil {
		t.Fatalf("growing reserve: %v", err)
	}
	if got := store.putCount(quotaKey); got != before+1 {
		t.Fatalf("growing reservation PUT count = %d, want %d", got, before+1)
	}
	if err := manager.reserve(ctx, request, "session-b", 120); err != nil {
		t.Fatalf("same-size new-session reserve: %v", err)
	}
	if got := store.putCount(quotaKey); got != before+2 {
		t.Fatalf("new-session reservation PUT count = %d, want %d", got, before+2)
	}

	store.quotaConflicts = 2
	conflictRequest := Request{Identity: "registry/conflict", Digest: "sha256:conflict"}
	if err := manager.reserve(ctx, conflictRequest, "session-c", 50); err != nil {
		t.Fatalf("reserve after CAS conflicts: %v", err)
	}
	if len(waits) != 2 || waits[0] != 0 || waits[1] != 1 {
		t.Fatalf("quota conflict waits = %v, want [0 1]", waits)
	}
	if quotaBackoffLimit(0) != 10*time.Millisecond || quotaBackoffLimit(5) != 250*time.Millisecond || quotaBackoffLimit(20) != 250*time.Millisecond {
		t.Fatal("quota conflict backoff limits are not capped as configured")
	}
}

type downloadFaultStore struct {
	storage.Store
	mu               sync.Mutex
	putCounts        map[string]int
	failStateOffset  int64
	failStateMode    string
	stateFailureUsed bool
	quotaFailureMode string
	quotaFailureUsed bool
	stateDelay       time.Duration
	slowStateOffset  int64
	quotaConflicts   int
}

func (s *downloadFaultStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.Entry{}, err
	}
	s.mu.Lock()
	if s.putCounts == nil {
		s.putCounts = make(map[string]int)
	}
	s.putCounts[key]++
	if key == quotaKey && s.quotaConflicts > 0 {
		s.quotaConflicts--
		s.mu.Unlock()
		return storage.Entry{}, storage.ErrConditionFailed
	}
	quotaMode := ""
	if key == quotaKey && s.quotaFailureMode != "" && !s.quotaFailureUsed {
		s.quotaFailureUsed = true
		quotaMode = s.quotaFailureMode
	}
	stateOffset := int64(-1)
	if strings.HasSuffix(key, "/state.json") {
		var value struct {
			Offset int64 `json:"offset"`
		}
		if json.Unmarshal(data, &value) == nil {
			stateOffset = value.Offset
		}
	}
	mode := ""
	if stateOffset == s.failStateOffset && s.failStateMode != "" && !s.stateFailureUsed {
		s.stateFailureUsed = true
		mode = s.failStateMode
	}
	delay := time.Duration(0)
	if stateOffset == s.slowStateOffset {
		delay = s.stateDelay
	}
	s.mu.Unlock()
	if quotaMode == "not-applied" {
		return storage.Entry{}, errors.Join(storage.ErrUnknownCommit, storage.ErrTimeout)
	}

	if mode == "error" || mode == "not-applied" {
		commitErr := storage.ErrTimeout
		if mode == "not-applied" {
			commitErr = storage.ErrUnknownCommit
		}
		return storage.Entry{}, errors.Join(commitErr, context.DeadlineExceeded)
	}
	if delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return storage.Entry{}, ctx.Err()
		case <-timer.C:
		}
	}
	entry, err := s.Store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
	if quotaMode == "applied" && err == nil {
		return entry, errors.Join(storage.ErrUnknownCommit, storage.ErrTimeout)
	}
	if mode == "applied" && err == nil {
		return entry, errors.Join(storage.ErrUnknownCommit, storage.ErrTimeout)
	}
	return entry, err
}

func (s *downloadFaultStore) putCount(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putCounts[key]
}

func TestCancelledFlushRequiresLifecycleAndGuard(t *testing.T) {
	for _, missing := range []string{"lifecycle", "guard"} {
		t.Run(missing, func(t *testing.T) {
			ctx, backing := testDownloadStore(t)
			manager := testManager(backing)
			content := []byte("abcdefgh")
			digest := artifact.CalculateSHA256(content)
			request := testRequest(content, digest, 44, nil)
			if missing == "lifecycle" {
				request.LifecycleContext = nil
			} else {
				request.Guard = nil
			}
			cancelled, cancel := context.WithCancel(ctx)
			request.Source = func(_ context.Context, _, _ int64, _ string) (*http.Response, context.CancelFunc, error) {
				return testResponse(http.StatusOK, content, int64(len(content)), &cancelAfterRead{reader: bytes.NewReader(content[:4]), cancel: cancel}), func() {}, nil
			}
			if _, err := manager.Fetch(cancelled, request); !errors.Is(err, context.Canceled) {
				t.Fatalf("Fetch() error = %v, want canceled", err)
			}
			stateData, _, err := manager.readState(ctx, stateKey(request.Identity, digest))
			if err != nil {
				t.Fatalf("read state: %v", err)
			}
			committed, err := decodeState(stateData, request)
			if err != nil || committed.Offset != 0 {
				t.Fatalf("state offset=%d error=%v, want no detached salvage", committed.Offset, err)
			}
		})
	}
}

func TestLifecycleCancellationInterruptsSalvageWrite(t *testing.T) {
	ctx, backing := testDownloadStore(t)
	lifecycle, cancelLifecycle := context.WithCancel(context.Background())
	defer cancelLifecycle()
	store := &cancelLifecycleStateStore{Store: backing, cancel: cancelLifecycle}
	manager := testManager(store)
	content := []byte("abcdefgh")
	digest := artifact.CalculateSHA256(content)
	request := testRequest(content, digest, 45, nil)
	request.LifecycleContext = lifecycle
	caller, cancelCaller := context.WithCancel(ctx)
	defer cancelCaller()
	calls := 0
	request.Source = func(_ context.Context, _, _ int64, _ string) (*http.Response, context.CancelFunc, error) {
		calls++
		return testResponse(http.StatusOK, content, int64(len(content)), &cancelAfterRead{reader: bytes.NewReader(content[:4]), cancel: cancelCaller}), func() {}, nil
	}

	_, err := manager.Fetch(caller, request)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Fetch() error = %v, want lifecycle cancellation", err)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	stateData, _, err := manager.readState(ctx, stateKey(request.Identity, digest))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	committed, err := decodeState(stateData, request)
	if err != nil || committed.Offset != 0 {
		t.Fatalf("state offset=%d error=%v, want lifecycle-cancelled candidate to remain uncommitted", committed.Offset, err)
	}
}

func TestSalvageFinalizationTimeoutBoundsStateWrite(t *testing.T) {
	ctx, backing := testDownloadStore(t)
	store := &downloadFaultStore{Store: backing, slowStateOffset: 4, stateDelay: 2 * time.Second}
	manager := testManager(store)
	content := []byte("abcdefgh")
	digest := artifact.CalculateSHA256(content)
	request := testRequest(content, digest, 46, nil)
	request.FinalizeTimeout = 30 * time.Millisecond
	caller, cancel := context.WithCancel(ctx)
	defer cancel()
	request.Source = func(_ context.Context, _, _ int64, _ string) (*http.Response, context.CancelFunc, error) {
		return testResponse(http.StatusOK, content, int64(len(content)), &cancelAfterRead{reader: bytes.NewReader(content[:4]), cancel: cancel}), func() {}, nil
	}

	started := time.Now()
	_, err := manager.Fetch(caller, request)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Fetch() error = %v, want finalization deadline", err)
	}
	if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
		t.Fatalf("salvage finalization took %s, want under 500ms", elapsed)
	}
	stateData, _, readErr := manager.readState(ctx, stateKey(request.Identity, digest))
	if readErr != nil {
		t.Fatalf("read state: %v", readErr)
	}
	committed, decodeErr := decodeState(stateData, request)
	if decodeErr != nil || committed.Offset != 0 {
		t.Fatalf("state offset=%d error=%v, want timed-out candidate to remain uncommitted", committed.Offset, decodeErr)
	}
}

func TestLeaseLossDuringSalvagePreventsChunkWrite(t *testing.T) {
	ctx, backing := testDownloadStore(t)
	manager := testManager(backing)
	content := []byte("abcdefgh")
	digest := artifact.CalculateSHA256(content)
	request := testRequest(content, digest, 47, nil)
	caller, cancel := context.WithCancel(ctx)
	defer cancel()
	salvageStarted := false
	salvageGuards := 0
	request.Guard = func(context.Context) error {
		if salvageStarted {
			salvageGuards++
			if salvageGuards == 2 {
				return storage.ErrLeaseLost
			}
		}
		return nil
	}
	calls := 0
	request.Source = func(_ context.Context, _, _ int64, _ string) (*http.Response, context.CancelFunc, error) {
		calls++
		return testResponse(http.StatusOK, content, int64(len(content)), &cancelAndSignalReader{
			reader: bytes.NewReader(content[:4]),
			cancel: cancel,
			signal: func() { salvageStarted = true },
		}), func() {}, nil
	}

	_, err := manager.Fetch(caller, request)
	var guardErr *GuardError
	if !errors.As(err, &guardErr) || !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("Fetch() error = %v, want lease-loss guard error", err)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	stateData, _, readErr := manager.readState(ctx, stateKey(request.Identity, digest))
	if readErr != nil {
		t.Fatalf("read state: %v", readErr)
	}
	committed, decodeErr := decodeState(stateData, request)
	if decodeErr != nil || committed.Offset != 0 || committed.Index != "" {
		t.Fatalf("state offset=%d index=%q error=%v, want no salvage commit", committed.Offset, committed.Index, decodeErr)
	}
	entries, _, listErr := backing.List(ctx, chunksPrefix(request.Identity, digest, committed.Session), "", 10)
	if listErr != nil || len(entries) != 0 {
		t.Fatalf("chunk objects=%v error=%v, want no chunk writes after guard loss", entries, listErr)
	}
}

type cancelLifecycleStateStore struct {
	storage.Store
	cancel context.CancelFunc
}

func (s *cancelLifecycleStateStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.Entry{}, err
	}
	if strings.HasSuffix(key, "/state.json") {
		var value struct {
			Offset int64 `json:"offset"`
		}
		if json.Unmarshal(data, &value) == nil && value.Offset > 0 {
			s.cancel()
			<-ctx.Done()
			return storage.Entry{}, ctx.Err()
		}
	}
	return s.Store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
}

type cancelAndSignalReader struct {
	reader io.Reader
	cancel context.CancelFunc
	signal func()
}

func (r *cancelAndSignalReader) Read(buffer []byte) (int, error) {
	n, _ := r.reader.Read(buffer)
	r.cancel()
	if r.signal != nil {
		r.signal()
	}
	return n, io.ErrUnexpectedEOF
}

func (*cancelAndSignalReader) Close() error { return nil }
