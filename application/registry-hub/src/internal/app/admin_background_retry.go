package app

import (
	"context"
	"errors"
	"net/http"

	"registry-hub/internal/auth"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

func (s *Service) retryBackground(writer http.ResponseWriter, request *http.Request, ctx context.Context, kind, id string) {
	q := s.backgroundQueue()
	q.ValidateRetrySource = s.validateRetrySource
	var result any
	var err error
	if request.Body != nil && request.ContentLength != 0 {
		var input workqueue.RetryRequest
		if decodeAdminBody(writer, request, &input) != nil {
			writeInventoryError(writer, inventory.ErrInvalid)
			return
		}
		principal, _ := auth.Authenticate(request, s.cfg.AdminAuth, false)
		result, err = q.AcceptRetry(ctx, principal.Name, kind, id, input)
	} else {
		// Preserve the original task-admin endpoint; conditional requests provide durable idempotency.
		principal, _ := auth.Authenticate(request, s.cfg.AdminAuth, false)
		if !principal.Allows(auth.TaskAdmin) {
			writeInventoryError(writer, inventory.ErrInvalid)
			return
		}
		err = q.Retry(ctx, kind, id)
		if err == nil {
			var job workqueue.Job
			job, err = q.Read(ctx, kind, id)
			result = diagnosticJob(job)
		}
		writer.Header().Set("Warning", "299 registry-hub \"Use expectedRevision, sourceRevision and idempotencyKey\"")
	}
	if errors.Is(err, storage.ErrConditionFailed) {
		writeAdminError(writer, http.StatusConflict, "REVISION_CONFLICT", "work changed or cannot be retried")
		return
	}
	if errors.Is(err, workqueue.ErrSourceMissing) {
		writeAdminError(writer, http.StatusConflict, "SOURCE_MISSING", "the authoritative source required by this work is no longer available")
		return
	}
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	writer.Header().Set("Location", "/api/v1/background/"+kind+"/"+id)
	writeJSON(writer, http.StatusAccepted, result)
}
