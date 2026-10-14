package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/config"
	"registry-hub/internal/storage"
	"sync"
	"time"
)

type gcRun struct {
	SchemaVersion int       `json:"schemaVersion"`
	ID            string    `json:"id"`
	Mode          string    `json:"mode"`
	State         string    `json:"state"`
	ExecutorID    string    `json:"executorID,omitempty"`
	JobName       string    `json:"jobName,omitempty"`
	JobUID        string    `json:"jobUID,omitempty"`
	StartedAt     time.Time `json:"startedAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
	CompletedAt   time.Time `json:"completedAt,omitempty"`
	ErrorClass    string    `json:"errorClass,omitempty"`
	ConfigDigest  string    `json:"configDigest,omitempty"`
	Lifecycle     bool      `json:"lifecycle,omitempty"`
	version       string
}

type gcController struct {
	service   *Service
	jobClient gcJobClient
	mu        sync.Mutex
	run       gcRun
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

type gcJobClient interface {
	createJob(context.Context, kubeJob) (jobStatus, error)
	getJob(context.Context, string) (jobStatus, error)
}

func newGCController(service *Service) (*gcController, error) {
	controller := &gcController{service: service}
	object, err := service.store.Get(context.Background(), "system/gc/last-run.json")
	if errors.Is(err, storage.ErrNotFound) {
		return controller, nil
	}
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
	_ = object.Body.Close()
	if readErr != nil || len(data) > 1<<20 {
		return nil, errors.New("GC run state could not be read safely")
	}
	if err := json.Unmarshal(data, &controller.run); err != nil || controller.run.SchemaVersion != 1 || controller.run.ID == "" {
		return nil, errors.New("GC run state is invalid")
	}
	controller.run.version = object.Version
	return controller, nil
}

func (c *gcController) Close() {
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Unlock()
	c.wg.Wait()
}

func (c *gcController) Snapshot() gcRun {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.run
}

func newGCRun(previous gcRun, cfg config.Config, id string, now time.Time) gcRun {
	configDigest := ""
	if cfg.GCMode == "kubernetes" {
		configDigest = gcJobConfigDigest(cfg)
	}
	return gcRun{
		SchemaVersion: 1,
		ID:            id,
		Mode:          cfg.GCMode,
		State:         "draining",
		ConfigDigest:  configDigest,
		Lifecycle:     cfg.GCLifecycleExecutable != "",
		StartedAt:     now,
		UpdatedAt:     now,
		version:       previous.version,
	}
}

func (c *gcController) Trigger(retry bool) (gcRun, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.service.cfg.GCMode == "disabled" {
		return gcRun{}, errors.New("GC execution is disabled")
	}
	if c.cancel != nil {
		return gcRun{}, errors.New("a GC executor is already active")
	}
	ctx, cancel := context.WithTimeout(c.service.ctx, 2*time.Hour)
	if retry {
		if err := c.service.gate.BeginRetry(ctx); err != nil {
			cancel()
			return gcRun{}, err
		}
	} else if err := c.service.gate.BeginDrain(ctx); err != nil {
		cancel()
		return gcRun{}, err
	}
	id, err := randomRunID()
	if err != nil {
		cancel()
		_ = c.service.gate.SetGCState(context.Background(), "failed_unknown", "", "random")
		return gcRun{}, err
	}
	now := time.Now().UTC()
	c.run = newGCRun(c.run, c.service.cfg, id, now)
	if err := c.persistLocked(ctx); err != nil {
		cancel()
		_ = c.service.gate.SetGCState(context.Background(), "failed_unknown", "", "storage")
		return gcRun{}, err
	}
	c.cancel = cancel
	c.wg.Add(1)
	go c.execute(ctx, id)
	return c.run, nil
}

func (c *gcController) execute(ctx context.Context, id string) {
	started := time.Now()
	defer func() {
		state := c.Snapshot().State
		result := "unknown"
		if state == "succeeded" {
			result = "success"
		} else if state == "failed" {
			result = "failure"
		}
		c.service.metrics.gcRuns.WithLabelValues(result).Inc()
		c.service.metrics.gcDuration.Observe(time.Since(started).Seconds())
	}()
	defer c.wg.Done()
	defer func() {
		c.mu.Lock()
		c.cancel = nil
		c.mu.Unlock()
	}()
	if err := c.service.gate.WaitQuiesced(ctx, 5*time.Minute); err != nil {
		c.fail("drain", "failed_unknown")
		return
	}
	if err := c.service.uploads.AbortAll(ctx); err != nil {
		c.fail("upload_drain", "failed_unknown")
		return
	}
	executorID := "local:" + id
	if c.service.cfg.GCMode == "kubernetes" {
		executorID = "kubernetes:" + c.service.cfg.GCNamespace + "/registry-hub-gc-" + id[:12]
	}
	if err := c.service.gate.SetGCState(ctx, "backend_quiesced", executorID, ""); err != nil {
		c.fail("storage", "failed_unknown")
		return
	}
	c.update("backend_quiesced", executorID, "", "")
	if err := c.service.gate.SetGCState(ctx, "gc_running", executorID, ""); err != nil {
		c.fail("storage", "failed_unknown")
		return
	}
	c.update("gc_running", executorID, "", "")
	lifecycleEnabled := c.service.cfg.GCLifecycleExecutable != ""
	if lifecycleEnabled && c.service.cfg.GCMode == "kubernetes" {
		if err := c.runLifecycle(ctx, "enter-readonly"); err != nil {
			c.fail("gc_lifecycle", "failed_unknown")
			return
		}
	}
	var err error
	if c.service.cfg.GCMode == "local" {
		err = c.runLocal(ctx)
	} else {
		err = c.runJob(ctx, executorID)
	}
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, errGCJobStatusUnknown) {
			c.fail("executor_unknown", "failed_unknown")
			return
		}
		c.fail("executor_failed", "failed")
		return
	}
	if lifecycleEnabled && c.service.cfg.GCMode == "kubernetes" {
		if err := c.runLifecycle(ctx, "exit-readonly"); err != nil {
			c.fail("gc_lifecycle", "failed_unknown")
			return
		}
	}
	if err := c.service.gate.SetGCState(ctx, "restore", executorID, ""); err != nil {
		c.fail("storage", "failed_unknown")
		return
	}
	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err = c.service.registry.Ping(pingCtx)
	cancel()
	if err != nil {
		c.fail("distribution_restore", "failed_unknown")
		return
	}
	if err := c.complete(ctx); err != nil {
		c.fail("storage", "failed_unknown")
		return
	}
	c.service.logger.Info("Distribution garbage collection completed", "gc_id", id)
}

func (c *gcController) complete(ctx context.Context) error {
	return c.completeWithState(ctx, "succeeded")
}

func (c *gcController) completeWithState(ctx context.Context, state string) error {
	c.mu.Lock()
	c.run.State = state
	c.run.CompletedAt = time.Now().UTC()
	c.run.UpdatedAt = c.run.CompletedAt
	if err := c.persistLocked(ctx); err != nil {
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()
	return c.service.gate.Open(ctx)
}
