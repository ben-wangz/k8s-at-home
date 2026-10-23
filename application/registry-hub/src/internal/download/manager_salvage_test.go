package download

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
)

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
