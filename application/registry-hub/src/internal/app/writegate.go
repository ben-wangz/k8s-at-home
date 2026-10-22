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
