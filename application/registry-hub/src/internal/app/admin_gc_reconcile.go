package app

import (
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/auth"
	"registry-hub/internal/registryapi"
)

func (s *Service) reconcileGCNoRun(writer http.ResponseWriter, request *http.Request, ctx context.Context, principal auth.Principal) {
	var input adminGCConfirmationRequest
	if err := decodeAdminBody(writer, request, &input); err != nil {
		writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "reconciliation confirmation is invalid")
		return
	}
	run, err := s.gc.ReconcileNoRun(ctx, input.ConfirmedNoExecutor)
	if err != nil {
		s.writeGCReconciliationError(writer, err)
		return
	}
	gate := s.gate.State()
	if s.logger != nil {
		s.logger.Info("GC coordinator state reconciled", "actor", principal.Name, "operation_id", requestID(), "gate_epoch", gate.Epoch)
	}
	if s.tasks != nil {
		processor := &solidifier{service: s, tasks: s.tasks, gate: s.gate}
		processor.wakeCoordinationWaiters(ctx)
		s.wakeAvailableQuotaWaiters(ctx, gate)
	}
	writeJSON(writer, http.StatusOK, adminGCStatusResponse{Gate: gate, Run: run, Sessions: []protectionSessionStatus{}})
}

func (s *Service) writeGCReconciliationError(writer http.ResponseWriter, err error) {
	var conflict *gcReconciliationConflict
	if errors.As(err, &conflict) {
		detail := adminGCConflictDetail{Reason: conflict.reason}
		if s.gate != nil {
			gate := s.gate.State()
			detail.Gate = &gate
		}
		registryapi.WriteErrorWithDetail(writer, http.StatusConflict, "GC_RECONCILIATION_BLOCKED", "GC reconciliation was refused", detail)
		return
	}
	writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "GC state could not be reconciled")
}
