package app

import (
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"slices"
	"strconv"
	"strings"
)

type backgroundJobPage struct {
	Items     []workqueue.Job `json:"items"`
	NextToken string          `json:"nextToken,omitempty"`
}

func (s *Service) serveBackground(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/api/v1/background/"), "/")
	if len(parts) < 1 || !slices.Contains(backgroundKinds, parts[0]) {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	q := workqueue.Queue{Store: s.store}
	writer.Header().Set("Cache-Control", "no-store")
	if len(parts) == 1 && request.Method == http.MethodGet {
		values := request.URL.Query()
		limit := 32
		if !onlyQueryKeys(values, "limit", "token") || !validAdminPageToken(values.Get("token"), 8192) {
			writeInventoryError(writer, inventory.ErrInvalid)
			return
		}
		if raw := values.Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > 32 {
				writeInventoryError(writer, inventory.ErrInvalid)
				return
			}
			limit = n
		}
		entries, next, err := storage.Scan(ctx, s.store, workqueue.Prefix+"jobs/"+parts[0]+"/", values.Get("token"), limit)
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		result := backgroundJobPage{Items: []workqueue.Job{}, NextToken: next}
		for _, entry := range entries {
			id := strings.TrimSuffix(entry.Key[strings.LastIndex(entry.Key, "/")+1:], ".json")
			job, err := q.Read(ctx, parts[0], id)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				writeInventoryError(writer, err)
				return
			}
			job.Payload, job.Progress = nil, nil
			result.Items = append(result.Items, job)
		}
		writeJSON(writer, http.StatusOK, result)
		return
	}
	if len(parts) < 2 || !inventory.ValidID(parts[1]) {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	if len(parts) == 2 && request.Method == http.MethodGet && len(request.URL.Query()) == 0 {
		job, err := q.Read(ctx, parts[0], parts[1])
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, job)
		return
	}
	if len(parts) == 3 && parts[2] == "retry" && request.Method == http.MethodPost && len(request.URL.Query()) == 0 {
		err := q.Retry(ctx, parts[0], parts[1])
		if errors.Is(err, storage.ErrConditionFailed) {
			writeAdminError(writer, http.StatusConflict, "UNAVAILABLE", "only failed or deferred work can be retried")
			return
		}
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		job, err := q.Read(ctx, parts[0], parts[1])
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		writeJSON(writer, http.StatusAccepted, job)
		return
	}
	writeInventoryError(writer, inventory.ErrInvalid)
}
