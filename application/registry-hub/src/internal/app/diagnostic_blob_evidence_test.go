package app

import (
	"context"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
	"time"
)

type blobEvidenceFailure struct {
	storage.Store
	prefix string
}

func (s blobEvidenceFailure) Get(ctx context.Context, key string) (storage.Object, error) {
	if strings.HasPrefix(key, s.prefix) {
		return storage.Object{}, storage.ErrUnavailable
	}
	return s.Store.Get(ctx, key)
}

func TestBlobDependencyDistinguishesCleanupAbsenceAndUncertainEvidence(t *testing.T) {
	for _, mode := range []string{"cleanup_pending", "deleted", "corrupt_marker", "corrupt_index", "version_changed", "verification_missing", "read_failure", "intent_failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newStagedAcceptanceFixture(t, 1<<20)
			ctx := context.Background()
			object := f.cacheObject([]byte("verified representation"), "application/octet-stream")
			root := tasks.Task{ID: "evidence", Authority: "ghcr.io", Repository: "team/image"}
			edge := tasks.DependencyEdge{ChildRole: tasks.GraphBlob, ChildDigest: object.Digest, Descriptor: tasks.GraphEntry{Size: object.Size, MediaType: testOCIConfigMediaType}}
			prefix := "cache/objects/" + cacheStorageIdentity(root.Authority, root.Repository, object.Digest) + "/"
			entries, _, err := storage.Scan(ctx, f.service.store, prefix, "", 32)
			if err != nil {
				t.Fatal(err)
			}
			want := "unknown"
			switch mode {
			case "cleanup_pending", "verification_missing":
				for _, entry := range entries {
					if !strings.Contains(entry.Key, "/representations/") {
						continue
					}
					if err := inventory.Update(ctx, f.service.store, entry.Key, func(value *map[string]any) error {
						if mode == "cleanup_pending" {
							(*value)["state"] = "cleanup_pending"
						} else {
							(*value)["verifiedAt"] = nil
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "cleanup_pending" {
					want = "complete"
				}
			case "deleted":
				if err := f.service.store.Delete(ctx, prefix+"data"); err != nil {
					t.Fatal(err)
				}
				want = "missing"
			case "version_changed":
				if _, err := f.service.store.Put(ctx, prefix+"data", strings.NewReader("replacement"), 11, storage.PutOptions{}); err != nil {
					t.Fatal(err)
				}
			case "corrupt_marker", "corrupt_index":
				for _, entry := range entries {
					match := strings.Contains(entry.Key, "/complete/")
					if mode == "corrupt_index" {
						match = strings.HasSuffix(entry.Key, "/representation-index.json")
					}
					if match {
						if _, err := f.service.store.Put(ctx, entry.Key, strings.NewReader("invalid"), 7, storage.PutOptions{}); err != nil {
							t.Fatal(err)
						}
					}
				}
			case "read_failure":
				f.service.cache = artifact.NewCache(blobEvidenceFailure{Store: f.service.store, prefix: prefix})
			case "intent_failure":
				if err := f.service.store.Delete(ctx, prefix+"data"); err != nil {
					t.Fatal(err)
				}
				f.service.records = tasks.NewRecords(blobEvidenceFailure{Store: f.service.store, prefix: "solidification/blobs/"})
			}
			view := f.service.taskDependency(ctx, root, edge)
			if view.CacheState != want {
				t.Fatalf("mode=%s view=%+v want=%s", mode, view, want)
			}
		})
	}
}

func TestBlobDependencyReportsTaskWaitAndPreservesRegistryEvidence(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	identity := tasks.BlobIdentity{Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("9", 64)}
	intent, _, err := f.service.records.EnsureBlobIntent(ctx, identity, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	intent, err = f.service.records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "cache-v1", 64)
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := f.service.tasks.EnqueueBlob(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	taskLease, err := f.service.tasks.Leases().Acquire(ctx, "tasks/"+task.ID, "dependency-wait-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	task.Status, task.WaitReason, task.WaitResourceKey = tasks.WaitingDeps, "object_lease_pending", "objects/ghcr.io/team/image"
	if err := f.service.tasks.SaveProgress(ctx, &task, taskLease); err != nil {
		t.Fatal(err)
	}
	_ = f.service.tasks.Leases().Release(ctx, taskLease)

	view := dependencyView{RegistryState: "unknown", CacheState: "missing", ReadErrors: map[string]string{}}
	f.service.enrichIntentDependency(ctx, intent, &view)
	if view.TaskObservation != "found" || view.TaskStatus != string(tasks.WaitingDeps) || view.TaskWaitReason != task.WaitReason ||
		view.WaitResourceKey != task.WaitResourceKey || view.Reason != "waiting_object_lease" || view.NextAction != "wait_object_lease" || view.TaskURL != "/api/v1/tasks/"+intent.ID {
		t.Fatalf("task wait evidence = %#v", view)
	}
	if !view.NextAttemptAt.Equal(intent.NextAttemptAt) || !view.LastProgressAt.Equal(intent.LastProgressAt) ||
		!view.TaskNextAttemptAt.Equal(task.NextAttemptAt) || !view.TaskLastProgressAt.Equal(task.LastProgressAt) {
		t.Fatalf("intent/task timing fields were conflated: %#v", view)
	}
	verified := dependencyView{RegistryState: "verified", CacheState: "missing", Reason: "available", NextAction: "none", ReadErrors: map[string]string{}}
	f.service.enrichIntentDependency(ctx, intent, &verified)
	if verified.Reason != "available" || verified.NextAction != "none" || verified.TaskObservation != "found" {
		t.Fatalf("task wait replaced current registry evidence: %#v", verified)
	}
}

func TestBlobDependencyReportsMissingInconsistentAndUnreadableTasks(t *testing.T) {
	for _, mode := range []string{"absent", "inconsistent", "unreadable"} {
		t.Run(mode, func(t *testing.T) {
			f := newStagedAcceptanceFixture(t, 1<<20)
			ctx := context.Background()
			identity := tasks.BlobIdentity{Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("8", 64)}
			intent, _, err := f.service.records.EnsureBlobIntent(ctx, identity, 0, "")
			if err != nil {
				t.Fatal(err)
			}
			if mode != "absent" {
				intent, err = f.service.records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "cache-v1", 64)
				if err != nil {
					t.Fatal(err)
				}
				task, _, err := f.service.tasks.EnqueueBlob(ctx, intent)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "inconsistent" {
					current, err := f.service.tasks.Get(ctx, task.ID)
					if err != nil {
						t.Fatal(err)
					}
					current.Repository = "other/image"
					currentLease, err := f.service.tasks.Leases().Acquire(ctx, "tasks/"+task.ID, "dependency-mismatch-test", time.Minute)
					if err != nil {
						t.Fatal(err)
					}
					if err := f.service.tasks.SaveProgress(ctx, &current, currentLease); err != nil {
						t.Fatal(err)
					}
					_ = f.service.tasks.Leases().Release(ctx, currentLease)
				} else {
					manager, err := tasks.NewManager(blobEvidenceFailure{Store: f.service.store, prefix: "tasks/state/"}, 1, 10, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer manager.Close()
					f.service.tasks = manager
				}
			}
			view := dependencyView{RegistryState: "unknown", ReadErrors: map[string]string{}}
			f.service.enrichIntentDependency(ctx, intent, &view)
			if view.TaskObservation != mode {
				t.Fatalf("task observation = %q, want %q (%#v)", view.TaskObservation, mode, view)
			}
			if mode == "inconsistent" && view.ReadErrors["task"] != "task_intent_mismatch" {
				t.Fatalf("task mismatch error = %q", view.ReadErrors["task"])
			}
			if mode == "unreadable" && view.ReadErrors["task"] == "" {
				t.Fatalf("unreadable task did not retain its read error: %#v", view)
			}
		})
	}
}
