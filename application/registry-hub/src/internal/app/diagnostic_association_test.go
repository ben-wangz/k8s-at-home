package app

import (
	"context"
	"encoding/json"
	"registry-hub/internal/artifact"
	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/inventory"
	"registry-hub/internal/workqueue"
	"strings"
	"testing"
	"time"
)

func TestIntentNotificationsDiscoverExcludedVariantsAndTargets(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	intent := newReadyBlobIntent(t, ctx, f.service.tasks, f.service.records)
	root := newDemandRootTask(t, ctx, f.service.tasks, "notification-target")
	linkRootBlobDemand(t, ctx, f.service.records, intent.ID, root.ID, strings.Repeat("a", 32), "scope", strings.Repeat("b", 32))
	intent, err := f.service.records.GetBlobIntent(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, excluded := range []string{"", root.ID} {
		if err := f.service.deferBlobNotification(ctx, intent.ID, excluded); err != nil {
			t.Fatal(err)
		}
	}
	guard := &inventoryWriteGuard{Store: f.service.store}
	f.service.store = guard
	path := "/api/v1/blob-intents/" + intent.ID + "/notifications?limit=1"
	seen := map[string]bool{}
	for pages := 0; pages < 3; pages++ {
		response := diagnosticGet(t, f.service, path)
		var page notificationWorkPage
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil {
			t.Fatalf("notification page=%d %s", response.Code, response.Body.String())
		}
		for _, item := range page.Items {
			if !item.MatchesCurrentIntent || item.TargetsURL == "" {
				t.Fatalf("notification=%+v", item)
			}
			seen[item.ExcludedRootID] = true
			targetsResponse := diagnosticGet(t, f.service, item.TargetsURL)
			var targets notificationTargetPage
			if targetsResponse.Code != 200 || json.Unmarshal(targetsResponse.Body.Bytes(), &targets) != nil || len(targets.Items) != 1 {
				t.Fatalf("targets=%d %s", targetsResponse.Code, targetsResponse.Body.String())
			}
			target := targets.Items[0]
			if target.RootTaskID != root.ID || target.Excluded != (item.ExcludedRootID != "") {
				t.Fatalf("target=%+v", target)
			}
		}
		if page.NextToken == "" {
			break
		}
		path = "/api/v1/blob-intents/" + intent.ID + "/notifications?limit=1&token=" + page.NextToken
	}
	if len(seen) != 2 || guard.writes != 0 {
		t.Fatalf("variants=%v writes=%d", seen, guard.writes)
	}
	jobID := workqueue.ID("notify", notificationSource(intent, ""))
	q := workqueue.Queue{Store: guard.Store}
	child, err := q.Enqueue(ctx, "wake_root", jobID+"/"+root.ID, root.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	child, err = q.Claim(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Finish(ctx, &child, nil, true, nil); err != nil {
		t.Fatal(err)
	}
	response := diagnosticGet(t, f.service, "/api/v1/blob-intents/"+intent.ID+"/notifications/"+jobID+"/targets")
	var targets notificationTargetPage
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &targets) != nil || len(targets.Items) != 1 || targets.Items[0].State != "done" {
		t.Fatalf("confirmation=%d %s", response.Code, response.Body.String())
	}
}

func TestImageDependencyParentSurvivesTaskGraphCleanup(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	configuration := f.cacheObject([]byte("parent edge configuration"), "application/vnd.oci.image.config.v1+json")
	layer := f.cacheObject([]byte("parent edge layer"), "application/vnd.oci.image.layer.v1.tar")
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &configuration, Layers: []artifact.Descriptor{layer}})
	root := f.root("parent-edges", manifest)
	_ = f.step(root.ID)
	snapshot := refreshTestInventory(t, f.service)
	if _, _, err := f.service.records.DeleteTaskGraph(ctx, root.ID, 100); err != nil {
		t.Fatal(err)
	}
	imageID := inventoryIdentity(root.Authority, root.Repository, root.Reference).ID()
	response := diagnosticGet(t, f.service, "/api/v1/images/"+imageID+"/dependencies?snapshotID="+snapshot.SnapshotID)
	var page imageDependencyPage
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil {
		t.Fatalf("dependencies=%d %s", response.Code, response.Body.String())
	}
	seen := map[string]bool{}
	for _, item := range page.Items {
		if item.Role == "manifest" {
			continue
		}
		if item.ParentDigest != manifest.Digest || len(item.ParentDigests) != 1 || item.ParentEdgesScope != "captured_manifest_metadata" || item.IntentID == "" || item.NotificationsURL == "" {
			t.Fatalf("dependency=%+v", item)
		}
		seen[item.Digest] = true
	}
	if !seen[configuration.Digest] || !seen[layer.Digest] {
		t.Fatalf("missing config or layer dependency: %v", seen)
	}
}

func TestParentEdgeCaptureBoundsSharedReferences(t *testing.T) {
	p := newInventoryClosureProgress("root")
	child := inventory.ObjectRef{Role: "layer", Digest: "shared"}
	for index := 0; index < 40; index++ {
		p.recordParent(child, inventory.ID("parent", string(rune(index))), 100)
	}
	key := closureParentKey(child)
	if len(p.Parents[key]) != 32 || !p.ParentsTruncated[key] {
		t.Fatalf("parents=%d truncated=%v", len(p.Parents[key]), p.ParentsTruncated[key])
	}
}
