package app

import (
	"net/http"
	"registry-hub/internal/auth"
	"registry-hub/internal/tasks"

	"github.com/danielgtaylor/huma/v2"
)

func adminOpenAPIOperations(api huma.API) []huma.Operation {
	taskResponse := adminJSONResponse("Task state.", adminSchema[tasks.Task](api))
	gcResponse := adminJSONResponse("Garbage collection state.", adminSchema[adminGCStatusResponse](api))
	gcRunResponse := adminJSONResponse("Garbage collection run.", adminSchema[gcRun](api))
	referrerResponse := adminJSONResponse("Referrer reconciliation state.", adminSchema[adminReferrerReconcileResponse](api))
	referrerActionSchema := &huma.Schema{AnyOf: []*huma.Schema{
		adminSchema[adminReferrerMutationResponse](api),
		adminSchema[adminReferrerReplaceResponse](api),
		adminSchema[adminReferrerMigrationResponse](api),
		adminSchema[adminReferrerRetryResponse](api),
	}}
	confirmationBody := adminJSONRequest("Explicitly confirm the GC action.", adminSchema[adminGCConfirmationRequest](api))
	return []huma.Operation{
		adminOperation(api, http.MethodGet, "/api/v1/tasks", "listTasks", "List tasks", auth.TaskAdmin,
			adminResponses(api, map[string]*huma.Response{"200": adminJSONResponse("Task page.", adminSchema[adminTaskListResponse](api))}, http.StatusBadRequest, http.StatusServiceUnavailable), adminTaskQueryParams()),
		adminOperation(api, http.MethodPost, "/api/v1/tasks/cleanup", "cleanupTasks", "Clean up expired task and cache state", auth.TaskAdmin,
			adminResponses(api, map[string]*huma.Response{"200": adminJSONResponse("Cleanup counts.", adminSchema[adminTaskCleanupResponse](api))}, http.StatusServiceUnavailable), nil),
		adminOperation(api, http.MethodGet, "/api/v1/tasks/{taskID}", "getTask", "Get a task", auth.TaskAdmin,
			adminResponses(api, map[string]*huma.Response{"200": taskResponse}, http.StatusNotFound, http.StatusServiceUnavailable), []*huma.Param{adminPathParam("taskID", "Task identifier.")}),
		adminOperation(api, http.MethodPost, "/api/v1/tasks/{taskID}/retry", "retryTask", "Retry a failed task", auth.TaskAdmin,
			adminResponses(api, map[string]*huma.Response{"202": taskResponse}, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable), []*huma.Param{adminPathParam("taskID", "Task identifier.")}),
		adminOperation(api, http.MethodPost, "/api/v1/tasks/{taskID}/cancel", "cancelTask", "Cancel a task", auth.TaskAdmin,
			adminResponses(api, map[string]*huma.Response{"202": taskResponse}, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable), []*huma.Param{adminPathParam("taskID", "Task identifier.")}),
		adminOperation(api, http.MethodGet, "/api/v1/gc", "getGCStatus", "Get garbage collection status", auth.GCAdmin,
			adminResponses(api, map[string]*huma.Response{"200": gcResponse}, http.StatusServiceUnavailable), nil),
		adminOperation(api, http.MethodPost, "/api/v1/gc", "startGC", "Start garbage collection", auth.GCAdmin,
			adminResponses(api, map[string]*huma.Response{"202": gcRunResponse}, http.StatusConflict, http.StatusServiceUnavailable), nil),
		adminOperation(api, http.MethodPost, "/api/v1/gc/retry", "retryGC", "Retry a failed garbage collection run", auth.GCAdmin,
			adminResponses(api, map[string]*huma.Response{"202": gcRunResponse}, http.StatusBadRequest, http.StatusConflict, http.StatusServiceUnavailable), nil, confirmationBody),
		adminOperation(api, http.MethodPost, "/api/v1/gc/recover", "recoverGC", "Recover garbage collection state", auth.GCAdmin,
			adminResponses(api, map[string]*huma.Response{"200": gcResponse}, http.StatusBadRequest, http.StatusConflict, http.StatusServiceUnavailable), nil, confirmationBody),
		adminOperation(api, http.MethodGet, "/api/v1/referrers/reconcile", "getReferrerReconciliation", "Read referrer indexes and repair state", auth.ReferrerPrune,
			adminResponses(api, map[string]*huma.Response{"200": referrerResponse}, http.StatusBadRequest, http.StatusBadGateway, http.StatusServiceUnavailable), adminReferrerQueryParams()),
		adminOperation(api, http.MethodPost, "/api/v1/referrers/reconcile", "reconcileReferrers", "Update or repair referrer indexes", auth.ReferrerPrune,
			adminResponses(api, map[string]*huma.Response{
				"200": adminJSONResponse("Completed referrer reconciliation.", referrerActionSchema),
				"202": adminJSONResponse("Scheduled referrer reconciliation.", referrerActionSchema),
			}, http.StatusBadRequest, http.StatusConflict, http.StatusBadGateway, http.StatusServiceUnavailable), nil,
			adminJSONRequest("Referrer reconciliation request.", adminSchema[referrerReconcileRequest](api))),
	}
}
