package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"time"
)

func (s *Service) runRetryRequestRetention(ctx context.Context) {
	s.runSourceSweep(ctx, "retry_request_retention", workqueue.Prefix+"retry-requests/", func(ctx context.Context, entry storage.Entry) error {
		if entry.ModTime == 0 || time.Since(time.Unix(0, entry.ModTime)) < 30*24*time.Hour {
			return nil
		}
		request, _, err := inventory.Read[workqueue.RetryAcceptance](ctx, s.store, entry.Key)
		if err != nil {
			return err
		}
		job, err := (workqueue.Queue{Store: s.store}).Read(ctx, request.Kind, request.WorkID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if err == nil && job.RetryRequestID == request.ID {
			return nil
		}
		return s.deleteDiagnosticObservation(ctx, entry)
	})
}
