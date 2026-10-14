package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/storage"
	"sync"
	"time"
)

var ErrWritesClosed = errors.New("Registry writes are blocked by garbage collection")

type gateRecord struct {
	SchemaVersion int       `json:"schemaVersion"`
	State         string    `json:"state"`
	Exclusive     bool      `json:"exclusive,omitempty"`
	StartedAt     time.Time `json:"startedAt,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
	ExecutorID    string    `json:"executorID,omitempty"`
	ErrorClass    string    `json:"errorClass,omitempty"`
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
		gate.state = gateRecord{SchemaVersion: 1, State: "open", UpdatedAt: time.Now().UTC()}
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
	if err := json.Unmarshal(data, &gate.state); err != nil || gate.state.SchemaVersion != 1 {
		return nil, errors.New("GC state has an unsupported schema")
	}
	if gate.state.State == "" {
		return nil, errors.New("GC state is missing its state")
	}
	gate.version = object.Version
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
	return state
}

func (g *writeGate) BeginDrain(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state.State != "open" || g.exclusive {
		return ErrWritesClosed
	}
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
		if g.active == 0 {
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
	g.state = gateRecord{SchemaVersion: 1, State: "open", UpdatedAt: time.Now().UTC()}
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
