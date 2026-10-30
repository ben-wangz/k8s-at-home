package app

import (
	"github.com/danielgtaylor/huma/v2"
	"net/http"
	"registry-hub/internal/auth"
)

func diagnosticParams(fields ...string) []*huma.Param {
	params := []*huma.Param{adminIntegerQueryParam("limit", "Maximum source records inspected per page. Filtered pages can be empty and still have a continuation.", 1, 32, 32), adminStringQueryParam("token", "Encrypted continuation bound to this route, filters and observation. Expired tokens return 410.", false)}
	for _, name := range fields {
		params = append(params, adminStringQueryParam(name, "Exact match filter: "+name+".", false))
	}
	return params
}

func diagnosticOperation[T any](api huma.API, path, id, summary string, params []*huma.Param) huma.Operation {
	return adminOperation(api, http.MethodGet, path, id, summary, auth.Observe, adminResponses(api, map[string]*huma.Response{"200": adminJSONResponse("Read-only observations. Unknown fields are not zero-valued evidence.", adminSchema[T](api))}, http.StatusBadRequest, http.StatusNotFound, http.StatusGone, http.StatusServiceUnavailable), params)
}

func adminDiagnosticOperations(api huma.API) []huma.Operation {
	return []huma.Operation{
		diagnosticOperation[notificationWorkPage](api, "/api/v1/blob-intents/{intentID}/notifications", "listIntentNotifications", "Read indexed notification variants and their revision coverage", append([]*huma.Param{adminPathParam("intentID", "Intent identifier.")}, diagnosticParams()...)),
		diagnosticOperation[notificationTargetPage](api, "/api/v1/blob-intents/{intentID}/notifications/{workID}/targets", "listNotificationTargets", "Read live notification target candidates and confirmed wake work", append([]*huma.Param{adminPathParam("intentID", "Intent identifier."), adminPathParam("workID", "Notification work identifier.")}, diagnosticParams()...)),
		diagnosticOperation[executionFactPage](api, "/api/v1/images/{imageID}/execution-facts", "listImageExecutionFacts", "Read durable execution facts independently of captured snapshot task IDs", append([]*huma.Param{adminPathParam("imageID", "Image identity; a published snapshot is not required.")}, diagnosticParams("digest", "generation")...)),
		diagnosticOperation[diagnosticPage[reservationView]](api, "/api/v1/download-reservations", "listDownloadReservations", "Read quota reservations including those without download state", diagnosticParams("digest", "sessionID", "stateEvidence")),
		diagnosticOperation[systemStatus](api, "/api/v1/status", "getSystemStatus", "Read independent component, capacity, worker and inventory status", nil),
		diagnosticOperation[workerList](api, "/api/v1/workers", "listWorkers", "Read process worker states and persisted scan observations", nil),
		diagnosticOperation[workerView](api, "/api/v1/workers/{workerID}", "getWorker", "Read a worker", []*huma.Param{adminPathParam("workerID", "Worker identifier.")}),
		diagnosticOperation[inventoryProjectionView](api, "/api/v1/images/{imageID}/projection", "getImageProjection", "Compare source, projected and captured revisions", []*huma.Param{adminPathParam("imageID", "Inventory image identifier."), adminStringQueryParam("snapshotID", "Retained snapshot identifier.", false)}),
		diagnosticOperation[imageDependencyPage](api, "/api/v1/images/{imageID}/dependencies", "getImageDependencies", "Read captured dependencies and direct task edge links", []*huma.Param{adminPathParam("imageID", "Inventory image identifier."), adminStringQueryParam("snapshotID", "Immutable retained inventory snapshot identifier.", false), adminIntegerQueryParam("limit", "Maximum captured objects per page.", 1, 32, 32), adminStringQueryParam("token", "Signed inventory continuation bound to this immutable snapshot and query, valid for 15 minutes. Expired tokens return 410.", false)}),
		diagnosticOperation[diagnosticPage[blobIntentView]](api, "/api/v1/blob-intents", "listBlobIntents", "List intents including those without tasks", diagnosticParams("state", "authority", "repository", "digest", "errorClass", "round")),
		diagnosticOperation[blobIntentView](api, "/api/v1/blob-intents/{intentID}", "getBlobIntent", "Read an intent and one bounded page of demands", append([]*huma.Param{adminPathParam("intentID", "Intent identifier.")}, diagnosticParams()...)),
		diagnosticOperation[dependencyPage](api, "/api/v1/tasks/{taskID}/dependencies", "getTaskDependencies", "Read direct graph edges and blockers without fetching payloads", append([]*huma.Param{adminPathParam("taskID", "Task identifier.")}, diagnosticParams()...)),
		diagnosticOperation[eventPage](api, "/api/v1/events", "listEvents", "Read bounded attempt, failure and recovery history", diagnosticParams(eventQueryFields...)),
		diagnosticOperation[diagnosticPage[transferView]](api, "/api/v1/transfers", "listTransfers", "Read independent transfer, cache commit, cleanup and quota observations", diagnosticParams("kind", "state", "authority", "repository", "digest", "taskID", "intentID", "sessionID", "cleanupState", "errorClass")),
		diagnosticOperation[transferView](api, "/api/v1/transfers/{transferID}", "getTransfer", "Read a transfer observation", []*huma.Param{adminPathParam("transferID", "Transfer identifier.")}),
		diagnosticOperation[backgroundJobPage](api, "/api/v1/background", "listAllBackgroundWork", "Enumerate active work and completed receipts across kinds", diagnosticParams(workQueryFields...)),
		diagnosticOperation[storageSummaryView](api, "/api/v1/storage/summary", "getStorageSummary", "Read physical object totals and declared collection coverage", diagnosticParams("snapshotID")),
		diagnosticOperation[storageUsagePage](api, "/api/v1/storage/usage", "listStorageUsage", "Read physical usage groups sorted by object bytes descending", diagnosticParams(storageUsageFilters...)),
		diagnosticOperation[storageObjectPage](api, "/api/v1/storage/objects", "listStorageObjects", "Read a bounded page from an immutable physical inventory", diagnosticParams(storageObjectFilters...)),
		diagnosticOperation[storageObject](api, "/api/v1/storage/objects/{objectID}", "getStorageObject", "Read a physical object observation", append([]*huma.Param{adminPathParam("objectID", "Object observation ID scoped to its storage snapshot. PhysicalID is stable across scans.")}, diagnosticParams("snapshotID")...)),
		diagnosticOperation[storageObjectPage](api, "/api/v1/storage/reclaimable", "previewReclaimableStorage", "Read conservative cleanup evidence; this is not deletion authorization", diagnosticParams(storageObjectFilters...)),
	}
}
