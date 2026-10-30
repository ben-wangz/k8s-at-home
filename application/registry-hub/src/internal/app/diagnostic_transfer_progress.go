package app

import (
	"context"
	"io"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/tasks"
	"sync/atomic"
	"time"
)

type transferAttempt struct {
	ID             string    `json:"id"`
	AttemptID      string    `json:"attemptID"`
	StartedAt      time.Time `json:"startedAt"`
	ProgressAt     time.Time `json:"progressAt"`
	FinishedAt     time.Time `json:"finishedAt"`
	BodyBytesRead  int64     `json:"bodyBytesRead"`
	TotalBytes     int64     `json:"totalBytes"`
	Committed      bool      `json:"committed"`
	HTTPStatus     int       `json:"httpStatus,omitempty"`
	ErrorClass     string    `json:"errorClass,omitempty"`
	EvidenceSource string    `json:"evidenceSource"`
}

type transferObserver struct {
	service *Service
	task    tasks.Task
	view    transferAttempt
	bytes   atomic.Int64
	stop    chan struct{}
	done    chan struct{}
}

type observedTransferReader struct {
	io.Reader
	observer *transferObserver
}

func (r observedTransferReader) Read(buffer []byte) (int, error) {
	n, err := r.Reader.Read(buffer)
	r.observer.bytes.Add(int64(n))
	return n, err
}

func (s *Service) observeDistributionTransfer(task tasks.Task, size int64) *transferObserver {
	session := task.RoundID
	if session == "" {
		session = task.ID
	}
	now := time.Now().UTC()
	id := inventory.ID("distribution_write", task.Authority, task.Repository, task.BlobDigest, session)
	o := &transferObserver{service: s, task: task, view: transferAttempt{ID: id, AttemptID: diagnostics.ID("distribution_upload", task.ID, now.Format(time.RFC3339Nano)), StartedAt: now, ProgressAt: now, TotalBytes: size, EvidenceSource: "HTTP transport body bytes read; not proof of remote acceptance; commit requires successful digest-qualified response"}, stop: make(chan struct{}), done: make(chan struct{})}
	o.persist()
	parent := s.ctx
	if parent == nil {
		parent = context.Background()
	}
	go func() {
		defer close(o.done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-o.stop:
				return
			case <-ticker.C:
				o.persist()
			case <-parent.Done():
				return
			}
		}
	}()
	return o
}

func (o *transferObserver) persist() {
	parent := o.service.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, time.Second)
	defer cancel()
	view := o.view
	view.BodyBytesRead = o.bytes.Load()
	if view.BodyBytesRead != o.view.BodyBytesRead || !view.FinishedAt.IsZero() {
		view.ProgressAt = time.Now().UTC()
	}
	o.view = view
	err := inventory.Update(ctx, o.service.store, diagnostics.Prefix+"transfer-progress/"+view.ID+".json", func(value *transferAttempt) error {
		if value.StartedAt.After(view.StartedAt) {
			return inventory.ErrSuperseded
		}
		*value = view
		return nil
	})
	if err != nil {
		o.service.diagnosticEventsDropped.Add(1)
	}
}

func (o *transferObserver) finish(cause error, status int, committed bool) {
	close(o.stop)
	<-o.done
	o.view.FinishedAt, o.view.HTTPStatus, o.view.Committed = time.Now().UTC(), status, committed
	if cause != nil && !committed {
		o.view.ErrorClass = diagnosticReason(cause)
	}
	o.persist()
	result := "complete"
	if !committed {
		result = "failed"
	}
	o.service.emitDiagnosticEvent(diagnostics.Event{Component: "distribution", Operation: "blob_upload", Stage: "transfer", Result: result, EntityID: o.view.ID, TransferID: o.view.ID, TaskID: o.task.ID, IntentID: o.task.ID, Authority: o.task.Authority, Repository: o.task.Repository, Digest: o.task.BlobDigest, AttemptID: o.view.AttemptID, AttemptLayer: "distribution_upload", StartedAt: o.view.StartedAt, FinishedAt: o.view.FinishedAt, ErrorClass: o.view.ErrorClass, HTTPStatus: status, Retryable: !committed})
}

func (s *Service) attachTransferAttempt(ctx context.Context, view *transferView) {
	if view.Kind != "distribution_write" {
		return
	}
	progress, _, err := inventory.Read[transferAttempt](ctx, s.store, diagnostics.Prefix+"transfer-progress/"+view.ID+".json")
	if err == nil {
		view.Attempt = &progress
	}
}
