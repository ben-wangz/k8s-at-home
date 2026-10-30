package app

import (
	"context"
	"encoding/json"
	"registry-hub/internal/artifact"
	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/inventory"
	"testing"
)

func TestDependencyQueryDoesNotMigrateLegacyGeneration(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	configuration := f.cacheObject([]byte("legacy config"), "application/vnd.oci.image.config.v1+json")
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &configuration, Layers: []artifact.Descriptor{}})
	root := f.root("legacy-query", manifest)
	_ = f.step(root.ID)
	edges, _, err := f.service.records.ListDependencyEdges(ctx, root.ID, "", 100)
	if err != nil || len(edges) == 0 {
		t.Fatalf("missing fixture dependencies: %v", err)
	}
	edge := edges[0]
	node, err := f.service.records.GetGraphNode(ctx, root.ID, edge.ChildRole, edge.ChildDigest)
	if err != nil {
		t.Fatal(err)
	}
	generation := uint64(4)
	node.ContentGeneration, node.GCEpoch, node.LocalState = &generation, f.service.gate.State().Epoch, "verified_present"
	if err := f.service.records.SaveGraphNode(ctx, &node); err != nil {
		t.Fatal(err)
	}
	key := generationKey(root.Authority, root.Repository, edge.ChildDigest)
	if err := inventory.Update(ctx, f.service.store, key, func(value *generationRecord) error {
		*value = generationRecord{SchemaVersion: generationSchemaPrevious, Authority: root.Authority, Repository: root.Repository, Reference: edge.ChildDigest, Generation: generation}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	guard := &inventoryWriteGuard{Store: f.service.store}
	f.service.store, f.service.inventory.Store = guard, guard
	response := diagnosticGet(t, f.service, "/api/v1/tasks/"+root.ID+"/dependencies")
	var page dependencyPage
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Items) == 0 {
		t.Fatalf("query failed: %d %s", response.Code, response.Body.String())
	}
	stored, _, err := inventory.Read[generationRecord](ctx, guard, key)
	if err != nil || guard.writes != 0 || stored.SchemaVersion != generationSchemaPrevious {
		t.Fatalf("GET migrated stored generation: writes=%d stored=%+v err=%v", guard.writes, stored, err)
	}
}
