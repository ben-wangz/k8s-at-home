package app

import (
	"context"
	"net/url"
	"registry-hub/internal/inventory"
	"registry-hub/internal/tasks"
	"testing"
)

func TestStorageMetadataCanBeQueriedByItsTaskAndImage(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	task := tasks.Task{ID: "00000000000000000000000000000001", Authority: "example.org", Repository: "team/image", Reference: "stable", RootDigest: "sha256:abc"}
	key := "tasks/state/" + task.ID + ".json"
	if err := inventory.Update(ctx, f.service.store, key, func(value *tasks.Task) error { *value = task; return nil }); err != nil {
		t.Fatal(err)
	}
	object := storageObject{Domain: "state", Category: "task_metadata", ObjectBytes: 10}
	if !f.service.inspectAssociatedMetadata(ctx, key, &object) || object.TaskID != task.ID || object.AssociationState != "known" || object.Repository != task.Repository {
		t.Fatalf("association=%+v", object)
	}
	id := inventoryIdentity(task.Authority, task.Repository, task.Reference).ID()
	if !matchesStorageAssociation(object, url.Values{"imageID": {id}}) {
		t.Fatal("known task metadata omitted from image filter")
	}
	state := newStorageCollection()
	addStorageUsage(&state, object)
	if state.Usage["state\x00image\x00"+id].ObjectBytes != 10 {
		t.Fatal("metadata omitted from image physical usage")
	}
}
