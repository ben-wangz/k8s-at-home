package app

import (
	"net/url"
	"registry-hub/internal/inventory"
	"registry-hub/internal/tasks"
	"strconv"
)

func matchesTaskDiagnostics(task tasks.Task, query url.Values) bool {
	imageID := inventoryIdentity(task.Authority, task.Repository, task.Reference).ID()
	contentID := inventory.Identity{Authority: task.Authority, Repository: task.Repository}.ContentID(task.RootDigest)
	fields := map[string]string{"kind": string(task.Kind), "waitReason": task.WaitReason, "errorClass": task.ErrorClass, "retryable": strconv.FormatBool(task.Retryable), "blobDigest": task.BlobDigest}
	for key, value := range fields {
		if query.Get(key) != "" && query.Get(key) != value {
			return false
		}
	}
	return query.Get("imageID") == "" || query.Get("imageID") == imageID || query.Get("imageID") == contentID
}
