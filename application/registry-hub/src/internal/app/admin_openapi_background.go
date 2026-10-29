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
	job := adminJSONResponse("Durable background work with source, progress, retries, and errors.", adminSchema[workqueue.Job](api))
	pageParams := append(append([]*huma.Param{}, params...), adminIntegerQueryParam("limit", "Maximum jobs per page.", 1, 32, 32), adminStringQueryParam("token", "Opaque storage continuation token.", false))
	return []huma.Operation{
		adminOperation(api, http.MethodGet, "/api/v1/background/{kind}", "listBackgroundWork", "List pending background work", auth.TaskAdmin, adminResponses(api, map[string]*huma.Response{"200": adminJSONResponse("One bounded page of background work headers.", adminSchema[backgroundJobPage](api))}, http.StatusBadRequest, http.StatusServiceUnavailable), pageParams),
		adminOperation(api, http.MethodGet, "/api/v1/background/{kind}/{jobID}", "getBackgroundWork", "Get background work", auth.TaskAdmin, adminResponses(api, map[string]*huma.Response{"200": job}, http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable), jobParams),
		adminOperation(api, http.MethodPost, "/api/v1/background/{kind}/{jobID}/retry", "retryBackgroundWork", "Retry failed background work", auth.TaskAdmin, adminResponses(api, map[string]*huma.Response{"202": job}, http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable), jobParams),
	}
}
