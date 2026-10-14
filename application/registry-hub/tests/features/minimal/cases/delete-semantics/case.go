package deletesemantics

import (
	"context"
	"embed"
	"fmt"
	"net/http"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "delete-semantics")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	registry := support.NewRegistryClient(environment)
	repository := support.LocalRepository("delete", environment.RunID)
	fixture, err := support.BuildImageFixture(ctx, registry, repository, "delete-a", environment.RunID+"-delete")
	if err != nil {
		return err
	}
	if err := putManifest(ctx, registry, repository, "delete-b", fixture.Manifest); err != nil {
		return err
	}
	response, err := registry.Delete(ctx, repository, "/manifests/delete-a")
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusAccepted {
		response.Body.Close()
		return fmt.Errorf("tag delete returned %s", response.Status)
	}
	response.Body.Close()
	if response, err = registry.Get(ctx, repository, "/manifests/delete-a", nil); err != nil {
		return err
	} else if response.StatusCode != http.StatusNotFound {
		response.Body.Close()
		return fmt.Errorf("deleted tag returned %s", response.Status)
	} else {
		response.Body.Close()
	}
	duplicateTagDelete, err := registry.Delete(ctx, repository, "/manifests/delete-a")
	if err != nil {
		return err
	}
	if duplicateTagDelete.StatusCode != http.StatusNotFound {
		duplicateTagDelete.Body.Close()
		return fmt.Errorf("repeated tag delete returned %s", duplicateTagDelete.Status)
	}
	duplicateTagDelete.Body.Close()
	if _, _, err := support.ReadManifest(ctx, registry, repository, fixture.ManifestDigest, support.OCIManifestMediaType); err != nil {
		return fmt.Errorf("digest disappeared after tag deletion: %w", err)
	}
	response, err = registry.Delete(ctx, repository, "/manifests/"+fixture.ManifestDigest)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusAccepted {
		response.Body.Close()
		return fmt.Errorf("digest delete returned %s", response.Status)
	}
	response.Body.Close()
	for _, reference := range []string{"delete-b", fixture.ManifestDigest} {
		response, err = registry.Get(ctx, repository, "/manifests/"+reference, nil)
		if err != nil {
			return err
		}
		if response.StatusCode == http.StatusOK {
			response.Body.Close()
			return fmt.Errorf("deleted manifest reference %s remains", reference)
		}
		response.Body.Close()
	}
	missingManifest, err := registry.Delete(ctx, repository, "/manifests/"+support.Digest([]byte("missing-manifest")))
	if err != nil {
		return err
	}
	if missingManifest.StatusCode != http.StatusNotFound {
		missingManifest.Body.Close()
		return fmt.Errorf("missing manifest delete returned %s", missingManifest.Status)
	}
	missingManifest.Body.Close()
	for digest, content := range fixture.Layers {
		if actual, readErr := support.ReadBlob(ctx, registry, repository, digest); readErr != nil || string(actual) != string(content) {
			return fmt.Errorf("manifest deletion cascaded to blob %s: %v", digest, readErr)
		}
	}
	deletedBlobContent := []byte("delete-blob:" + environment.RunID)
	deletedBlobDigest, err := registry.PutBlob(ctx, repository, deletedBlobContent)
	if err != nil {
		return err
	}
	deletedBlob, err := registry.Delete(ctx, repository, "/blobs/"+deletedBlobDigest)
	if err != nil {
		return err
	}
	if deletedBlob.StatusCode != http.StatusAccepted {
		deletedBlob.Body.Close()
		return fmt.Errorf("existing blob delete returned %s", deletedBlob.Status)
	}
	deletedBlob.Body.Close()
	deletedBlobRead, err := registry.Get(ctx, repository, "/blobs/"+deletedBlobDigest, nil)
	if err != nil {
		return err
	}
	if deletedBlobRead.StatusCode != http.StatusNotFound {
		deletedBlobRead.Body.Close()
		return fmt.Errorf("deleted blob read returned %s", deletedBlobRead.Status)
	}
	deletedBlobRead.Body.Close()
	missing, err := registry.Delete(ctx, repository, "/blobs/"+support.Digest([]byte("missing")))
	if err != nil {
		return err
	}
	if missing.StatusCode != http.StatusNotFound {
		missing.Body.Close()
		return fmt.Errorf("missing blob delete returned %s", missing.Status)
	}
	missing.Body.Close()
	return support.WriteResult(environment, map[string]any{"repository": repository, "deletedTag": "delete-a", "repeatedTagDelete": http.StatusNotFound, "deletedDigest": fixture.ManifestDigest, "missingManifestDelete": http.StatusNotFound, "deletedBlobDigest": deletedBlobDigest, "retainedBlobDigest": fixture.ConfigDigest})
}

func putManifest(ctx context.Context, registry *support.RegistryClient, repository, reference string, content []byte) error {
	response, _, err := registry.PutManifest(ctx, repository, reference, support.OCIManifestMediaType, content, nil)
	if response != nil {
		response.Body.Close()
	}
	return err
}
