package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"registry-hub/internal/artifact"
	"registry-hub/internal/tasks"
	"testing"
)

func TestSolidificationPerformanceLogsPlatformAnchorBeforeRootCompletion(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	var output bytes.Buffer
	f.service.logger = slog.New(slog.NewJSONHandler(&output, nil))
	amd := f.cacheObject([]byte("amd64 performance config"), "application/vnd.oci.image.config.v1+json")
	armBody := []byte("arm64 performance config")
	arm := artifact.Descriptor{Digest: artifact.CalculateSHA256(armBody), Size: int64(len(armBody)), MediaType: amd.MediaType}
	amdManifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &amd, Layers: []artifact.Descriptor{}})
	armManifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &arm, Layers: []artifact.Descriptor{}})
	amdManifest.Platform = json.RawMessage(`{"os":"linux","architecture":"amd64"}`)
	armManifest.Platform = json.RawMessage(`{"os":"linux","architecture":"arm64"}`)
	children := []artifact.Descriptor{amdManifest, armManifest}
	rootDescriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociIndexMediaType, Manifests: &children})
	root := f.root("performance", rootDescriptor)
	wait := func() {
		t.Helper()
		var yielded *tasks.Yield
		if err := f.step(root.ID); !errors.As(err, &yielded) || yielded.Status != tasks.WaitingDeps {
			t.Fatalf("root step = %v, want dependency wait", err)
		}
	}
	wait()
	wait()
	if output.Len() != 0 {
		t.Fatalf("routine waits emitted logs: %s", output.String())
	}
	f.transfer(amd)
	wait()
	anchors := solidificationLogEntries(t, &output, "solidification manifest anchored")
	if len(anchors) != 1 || anchors[0]["digest"] != amdManifest.Digest || anchors[0]["anchor_age_seconds"].(float64) <= 0 {
		t.Fatalf("selected platform anchor missing: %s", output.String())
	}
	if len(solidificationLogEntries(t, &output, "solidification root completed")) != 0 {
		t.Fatal("incomplete multi-platform root reported completion")
	}
	before := output.Len()
	wait()
	if output.Len() != before {
		t.Fatalf("repeated wait logged an already anchored platform: %s", output.String())
	}
	f.cacheObject(armBody, "application/octet-stream")
	wait()
	f.transfer(arm)
	if err := f.step(root.ID); err != nil {
		t.Fatal(err)
	}
	entries := solidificationLogEntries(t, &output, "solidification root completed")
	if len(entries) != 1 {
		t.Fatalf("want one root completion: %s", output.String())
	}
	entry := entries[0]
	wantBytes := rootDescriptor.Size + amdManifest.Size + armManifest.Size + amd.Size + arm.Size
	if entry["root_digest"] != rootDescriptor.Digest || entry["manifest_count"] != float64(3) || entry["blob_count"] != float64(2) || entry["closure_bytes"] != float64(wantBytes) || entry["discovery_reused"] != true {
		t.Fatalf("incorrect closure summary: %v", entry)
	}
	if entry["task_age_seconds"].(float64) < entry["execution_seconds"].(float64) || entry["discovery_seconds"].(float64) <= 0 {
		t.Fatalf("task age must include earlier dependency waits: %v", entry)
	}
	if len(solidificationLogEntries(t, &output, "solidification manifest anchored")) != 3 || len(solidificationLogEntries(t, &output, "solidification blob verified")) != 2 || len(solidificationLogEntries(t, &output, "solidification failed")) != 0 {
		t.Fatalf("unexpected completion/failure volume: %s", output.String())
	}
}
