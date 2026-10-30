package app

import (
	"github.com/danielgtaylor/huma/v2"
	"net/http"
	"registry-hub/internal/auth"
	"registry-hub/internal/workqueue"
)

func adminBackgroundOperations(api huma.API) []huma.Operation {
	params := []*huma.Param{adminPathParam("kind", "Background work kind: inventory, execution, download_cleanup, notify, or wake_root.")}
	jobParams := append(append([]*huma.Param{}, params...), adminPathParam("jobID", "Persistent background work ID."))
	job := adminJSONResponse("Durable background work with opaque revisions and safe resource associations.", adminSchema[backgroundJobView](api))
	pageParams := append(append([]*huma.Param{}, params...), diagnosticParams(workQueryFields...)...)
	retryBody := adminJSONRequest("Conditional retry using exact work and source revisions. HTTP 409 REVISION_CONFLICT means stale work, stale source, ineligible state or conflicting idempotency input; SOURCE_MISSING means the required authoritative source is absent. HTTP 503 means validation could not complete. An empty body remains available to legacy task-admin clients.", adminSchema[workqueue.RetryRequest](api))
	retryBody.Required = false
	retryResponse := &huma.Schema{OneOf: []*huma.Schema{adminSchema[workqueue.RetryAcceptance](api), adminSchema[backgroundJobView](api)}}
	return []huma.Operation{
		adminOperation(api, http.MethodGet, "/api/v1/background/{kind}", "listBackgroundWork", "List active background work and retained completion receipts", auth.Observe, adminResponses(api, map[string]*huma.Response{"200": adminJSONResponse("One bounded page; state includes pending, running, waiting_dependency, retry_wait, failed, done and superseded.", adminSchema[backgroundJobPage](api))}, http.StatusBadRequest, http.StatusGone, http.StatusServiceUnavailable), pageParams),
		adminOperation(api, http.MethodGet, "/api/v1/background/{kind}/{jobID}", "getBackgroundWork", "Get background work", auth.Observe, adminResponses(api, map[string]*huma.Response{"200": job}, http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable), jobParams),
		adminOperation(api, http.MethodPost, "/api/v1/background/{kind}/{jobID}/retry", "retryBackgroundWork", "Reliably accept a retry conditional on work and source revisions", auth.WorkRetry, adminResponses(api, map[string]*huma.Response{"202": adminJSONResponse("Accepted conditional retry or legacy work header; follow statusURL for execution. Duplicate idempotency keys return the original acceptance.", retryResponse)}, http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable), jobParams, retryBody),
	}
}
