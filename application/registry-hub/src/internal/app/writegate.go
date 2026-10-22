package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"registry-hub/internal/storage"
	"sync"
	"time"
)

var ErrWritesClosed = errors.New("Registry writes are blocked by garbage collection")
var ErrGCProtected = errors.New("GC is blocked by active solidification protection")
var ErrProtectionCapacity = errors.New("solidification protection capacity is exhausted")
var ErrProtectedCacheCapacity = errors.New("solidification protected cache capacity is exhausted")
var ErrRecoveryBarrier = errors.New("solidification recovery barrier is active")
var ErrProtectionAnchored = errors.New("solidification protection was already anchored")
var ErrProtectionBusy = errors.New("solidification protection has active operations")
var ErrProtectionHasContent = errors.New("solidification protection may contain unanchored content")

const (
	gateSchemaPrevious = 1
	gateSchemaLegacy   = 2
	gateSchemaInterim  = 3
	gateSchemaCurrent  = 4
)

type gateRecord struct {
	SchemaVersion                  int              `json:"schemaVersion"`
	State                          string           `json:"state"`
	Exclusive                      bool             `json:"exclusive,omitempty"`
	StartedAt                      time.Time        `json:"startedAt,omitempty"`
	UpdatedAt                      time.Time        `json:"updatedAt"`
	ExecutorID                     string           `json:"executorID,omitempty"`
	ErrorClass                     string           `json:"errorClass,omitempty"`
	Epoch                          uint64           `json:"epoch"`
	SessionIDs                     []string         `json:"sessionIDs,omitempty"`
	ReservedDistributionBytes      int64            `json:"reservedDistributionBytes"`
	ProtectedCacheBytes            int64            `json:"protectedCacheBytes"`
	ProtectedCacheReservations     map[string]int64 `json:"protectedCacheReservations,omitempty"`
	ProtectedCacheReservationCount int              `json:"protectedCacheReservationCount,omitempty"`
	LegacyGrandfatheredBytes       int64            `json:"legacyGrandfatheredBytes,omitempty"`
	RecoveryBarrier                bool             `json:"recoveryBarrier,omitempty"`
	RecoveryReason                 string           `json:"recoveryReason,omitempty"`
	ProtectionSessionCount         int              `json:"protectionSessionCount,omitempty"`
	NewProtectionSessionCount      int              `json:"newProtectionSessionCount,omitempty"`
}

type writeGate struct {
	store     storage.Store
	mu        sync.Mutex
	changed   chan struct{}
	state     gateRecord
	version   string
	active    int
	exclusive bool
}

func newWriteGate(ctx context.Context, store storage.Store) (*writeGate, error) {
	gate := &writeGate{store: store, changed: make(chan struct{})}
	object, err := store.Get(ctx, "system/gc/state.json")
	if errors.Is(err, storage.ErrNotFound) {
		gate.state = gateRecord{SchemaVersion: gateSchemaCurrent, State: "open", Epoch: 1, UpdatedAt: time.Now().UTC(), ProtectedCacheReservations: make(map[string]int64)}
		if err := gate.persist(ctx); err != nil {
			return nil, err
		}
		return gate, nil
	}
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
	_ = object.Body.Close()
	if readErr != nil || len(data) > 1<<20 {
		return nil, errors.New("GC state could not be read safely")
	}
	if err := json.Unmarshal(data, &gate.state); err != nil {
		return nil, errors.New("GC state has an unsupported schema")
	}
	if gate.state.SchemaVersion != gateSchemaPrevious && gate.state.SchemaVersion != gateSchemaLegacy && gate.state.SchemaVersion != gateSchemaInterim && gate.state.SchemaVersion != gateSchemaCurrent {
		return nil, errors.New("GC state has an unsupported schema")
	}
	loadedSchema := gate.state.SchemaVersion
	if gate.state.State == "" {
		return nil, errors.New("GC state is missing its state")
	}
	gate.version = object.Version
	if gate.state.ProtectedCacheReservations == nil {
		gate.state.ProtectedCacheReservations = make(map[string]int64)
	}
	if gate.state.SchemaVersion != gateSchemaCurrent {
		gate.state.NewProtectionSessionCount = 0
		for _, sessionID := range gate.state.SessionIDs {
			session, err := gate.readProtectionSession(ctx, sessionID)
			if err != nil {
				return nil, fmt.Errorf("migrate protection session accounting: %w", err)
			}
			if session.ReservationClass == "new" {
				gate.state.NewProtectionSessionCount++
			}
		}
		gate.state.SchemaVersion = gateSchemaCurrent
		if gate.state.Epoch == 0 {
			gate.state.Epoch = 1
		}
		gate.state.ProtectedCacheReservationCount = len(gate.state.ProtectedCacheReservations)
		if err := gate.persist(ctx); err != nil {
			return nil, fmt.Errorf("migrate GC coordinator state: %w", err)
		}
	}
	if gate.state.Epoch == 0 || gate.state.ReservedDistributionBytes < 0 || gate.state.ProtectedCacheBytes < 0 || gate.state.LegacyGrandfatheredBytes < 0 || gate.state.NewProtectionSessionCount < 0 || len(gate.state.SessionIDs) > 4096 || len(gate.state.ProtectedCacheReservations) > 10000 {
		return nil, errors.New("GC coordinator state is invalid")
	}
	if loadedSchema == gateSchemaCurrent && gate.state.ProtectedCacheReservationCount != len(gate.state.ProtectedCacheReservations) {
		return nil, errors.New("GC coordinator protected cache reservation count is invalid")
	}
	gate.state.ProtectionSessionCount = len(gate.state.SessionIDs)
	gate.state.ProtectedCacheReservationCount = len(gate.state.ProtectedCacheReservations)
	return gate, nil
}

func (g *writeGate) Enter(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	if g.state.State != "open" || g.exclusive {
		g.mu.Unlock()
		return nil, ErrWritesClosed
	}
	if g.state.RecoveryBarrier {
		g.mu.Unlock()
		return nil, ErrRecoveryBarrier
	}
	g.active++
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.active--
			g.signalLocked()
			g.mu.Unlock()
		})
	}, nil
}

func (g *writeGate) State() gateRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	state := g.state
	state.Exclusive = g.exclusive
	state.ProtectionSessionCount = len(g.state.SessionIDs)
	state.SessionIDs = nil
	state.ProtectedCacheReservationCount = len(g.state.ProtectedCacheReservations)
	state.ProtectedCacheReservations = nil
	return state
}

func (g *writeGate) BeginDrain(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state.State != "open" || g.exclusive {
		return ErrWritesClosed
	}
	if g.state.RecoveryBarrier {
		return ErrRecoveryBarrier
	}
	if err := g.validateProtectionStateLocked(ctx, false); err != nil {
		return err
	}
	if len(g.state.SessionIDs) > 0 || g.state.ReservedDistributionBytes != 0 {
		return ErrGCProtected
	}
	if g.state.Epoch == ^uint64(0) {
		return errors.New("GC epoch is exhausted")
	}
	g.state.Epoch++
	g.state.State = "draining"
	g.state.StartedAt = time.Now().UTC()
	g.state.UpdatedAt = g.state.StartedAt
	if err := g.persistLocked(ctx); err != nil {
		g.state.State = "failed_unknown"
		g.signalLocked()
		return err
	}
	g.signalLocked()
	return nil
}

func (g *writeGate) BeginRetry(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.exclusive || g.state.State != "failed_unknown" && g.state.State != "failed" {
		return errors.New("GC retry requires a failed or unknown state")
	}
	if g.state.RecoveryBarrier {
		return ErrRecoveryBarrier
	}
	if err := g.validateProtectionStateLocked(ctx, false); err != nil {
		return err
	}
	if len(g.state.SessionIDs) > 0 || g.state.ReservedDistributionBytes != 0 {
		return ErrGCProtected
	}
	if g.state.Epoch == ^uint64(0) {
		return errors.New("GC epoch is exhausted")
	}
	g.state.Epoch++
	g.state.State = "draining"
	g.state.ErrorClass = ""
	g.state.UpdatedAt = time.Now().UTC()
	if err := g.persistLocked(ctx); err != nil {
		g.state.State = "failed_unknown"
		g.state.ErrorClass = "storage"
		g.signalLocked()
		return err
	}
	g.signalLocked()
	return nil
}

func (g *writeGate) BeginExclusive(ctx context.Context, timeout time.Duration) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	g.mu.Lock()
	if g.state.State != "open" || g.exclusive {
		g.mu.Unlock()
		return nil, ErrWritesClosed
	}
	g.exclusive = true
	g.signalLocked()
	for g.active > 0 {
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			g.clearExclusive()
			return nil, ctx.Err()
		case <-deadline.C:
			g.clearExclusive()
			return nil, errors.New("timed out draining writes for exclusive operation")
		case <-changed:
		}
		g.mu.Lock()
	}
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(g.clearExclusive)
	}, nil
}

func (g *writeGate) clearExclusive() {
	g.mu.Lock()
	g.exclusive = false
	g.signalLocked()
	g.mu.Unlock()
}

func (g *writeGate) WaitQuiesced(ctx context.Context, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		g.mu.Lock()
		if g.state.State != "draining" {
			state := g.state.State
			g.mu.Unlock()
			return errors.New("GC gate left draining state: " + state)
		}
		if g.active == 0 && len(g.state.SessionIDs) == 0 && g.state.ReservedDistributionBytes == 0 {
			g.state.State = "backend_quiesced"
			g.state.UpdatedAt = time.Now().UTC()
			err := g.persistLocked(ctx)
			if err != nil {
				g.state.State = "failed_unknown"
				g.state.ErrorClass = "storage"
			}
			g.signalLocked()
			g.mu.Unlock()
			return err
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("timed out draining active Registry writes")
		case <-changed:
		}
	}
}

func (g *writeGate) WaitRecoveryQuiesced(ctx context.Context, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		g.mu.Lock()
		if !g.state.RecoveryBarrier {
			g.mu.Unlock()
			return errors.New("recovery barrier is not active")
		}
		if g.active == 0 {
			g.mu.Unlock()
			return nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("timed out waiting for active Registry operations before recovery")
		case <-changed:
		}
	}
}

func (g *writeGate) SetGCState(ctx context.Context, state, executorID, errorClass string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.state.State = state
	g.state.ExecutorID = executorID
	g.state.ErrorClass = errorClass
	g.state.UpdatedAt = time.Now().UTC()
	if err := g.persistLocked(ctx); err != nil {
		g.state.State = "failed_unknown"
		g.state.ErrorClass = "storage"
		g.signalLocked()
		return err
	}
	g.signalLocked()
	return nil
}

func (g *writeGate) Open(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active != 0 || g.state.State != "restore" {
		return errors.New("GC gate cannot be opened from its current state")
	}
	if g.state.RecoveryBarrier || len(g.state.SessionIDs) != 0 || g.state.ReservedDistributionBytes != 0 {
		return ErrGCProtected
	}
	g.state.State = "open"
	g.state.StartedAt = time.Time{}
	g.state.ErrorClass = ""
	g.state.RecoveryReason = ""
	g.state.ExecutorID = ""
	g.state.UpdatedAt = time.Now().UTC()
	if err := g.persistLocked(ctx); err != nil {
		g.state.State = "failed_unknown"
		g.state.ErrorClass = "storage"
		return err
	}
	g.signalLocked()
	return nil
}

func (g *writeGate) persist(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.persistLocked(ctx)
}
