package referrersmigration

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	referrersfallback "github.com/ben-wangz/registry-hub-tests/features/referrers-fallback"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"strings"
)

//go:embed case.json
var metadata embed.FS

const (
	legacyArtifactType = "application/vnd.example.legacy"
	nativeArtifactType = "application/vnd.example.native"
)

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "referrers-migration")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	fixture, err := installZotFixture(ctx, environment)
	if err != nil {
		return err
	}
	registry := support.NewRegistryClient(environment)
	repository := support.LocalRepository(environment.CaseID, environment.RunID)
	subject, err := support.BuildImageFixture(ctx, registry, repository, "subject", environment.RunID)
	if err != nil {
		return err
	}
	tagsStatus, nativeStatus, err := referrersfallback.ProbeNativeReferrers(ctx, environment, repository, subject.ManifestDigest)
	if err != nil {
		return err
	}
	if tagsStatus != http.StatusOK || nativeStatus != http.StatusOK {
		return fmt.Errorf("native Registry probe returned tags=%d referrers=%d, want 200/200", tagsStatus, nativeStatus)
	}

	legacy, legacyDigest, err := support.BuildReferrer(ctx, registry, repository, "legacy", legacyArtifactType, subject.ManifestDigest)
	if err != nil {
		return err
	}
	_, nativeDigest, err := support.BuildReferrer(ctx, registry, repository, "native-only", nativeArtifactType, subject.ManifestDigest)
	if err != nil {
		return err
	}
	legacyDescriptor, err := referrerDescriptor(legacy, legacyDigest, subject.ManifestDigest)
	if err != nil {
		return err
	}
	legacyTag := fallbackTag(subject.ManifestDigest)
	fallbackIndexDigest, err := publishLegacyIndex(ctx, registry, repository, legacyTag, legacyDescriptor)
	if err != nil {
		return err
	}

	before, err := readReferrers(ctx, registry, repository, subject.ManifestDigest)
	if err != nil {
		return err
	}
	if err := requireDescriptors(before.Manifests, map[string]string{legacyDigest: legacyArtifactType}); err != nil {
		return fmt.Errorf("pre-migration authority: %w", err)
	}
	migration, err := migrate(ctx, environment, repository, subject.ManifestDigest)
	if err != nil {
		return err
	}
	if migration.Action != "migrate" || !migration.Complete || migration.Generation == 0 ||
		migration.Referrers != 1 || migration.FallbackIndexDigest != fallbackIndexDigest ||
		!validDigest(migration.NativeIndexDigest) {
		return fmt.Errorf("migration response is incomplete or does not identify the seeded fallback index")
	}

	after, err := readReferrers(ctx, registry, repository, subject.ManifestDigest)
	if err != nil {
		return err
	}
	if err := requireDescriptors(after.Manifests, map[string]string{
		legacyDigest: legacyArtifactType,
		nativeDigest: nativeArtifactType,
	}); err != nil {
		return fmt.Errorf("post-migration native authority: %w", err)
	}
	migrationState, err := readMigrationState(ctx, environment, repository, subject.ManifestDigest)
	if err != nil {
		return err
	}
	if migrationState.State != "complete" || migrationState.Generation != migration.Generation {
		return fmt.Errorf("migration state is %q at generation %d, want complete at generation %d", migrationState.State, migrationState.Generation, migration.Generation)
	}

	retained, err := readManifestIndex(ctx, registry, repository, legacyTag)
	if err != nil {
		return err
	}
	if err := requireDescriptors(retained.Manifests, map[string]string{legacyDigest: legacyArtifactType}); err != nil {
		return fmt.Errorf("retained fallback index: %w", err)
	}
	deleteResponse, err := registry.Delete(ctx, repository, "/manifests/"+legacyTag)
	if err != nil {
		return err
	}
	deleteStatus := deleteResponse.StatusCode
	deleteResponse.Body.Close()
	if deleteStatus != http.StatusConflict {
		return fmt.Errorf("protected fallback tag deletion returned %d, want 409", deleteStatus)
	}

	if err := fixture.Restore(ctx); err != nil {
		return err
	}
	restored, err := registry.Head(ctx, repository, "/manifests/"+subject.ManifestDigest, map[string]string{"Accept": support.OCIManifestMediaType})
	if err != nil {
		return fmt.Errorf("verify Registry Service restoration: %w", err)
	}
	restoredStatus := restored.StatusCode
	restored.Body.Close()
	if restoredStatus != http.StatusNotFound {
		return fmt.Errorf("restored Chart Registry returned %d for the Zot-only subject, want 404", restoredStatus)
	}
	return support.WriteResult(environment, map[string]any{
		"fixtureImage": zotImageReference(), "repository": repository, "subjectDigest": subject.ManifestDigest,
		"nativeTagsStatus": tagsStatus, "nativeReferrersStatus": nativeStatus,
		"legacyReferrerDigest": legacyDigest, "nativeReferrerDigest": nativeDigest,
		"fallbackTag": legacyTag, "fallbackIndexDigest": fallbackIndexDigest,
		"nativeIndexDigest": migration.NativeIndexDigest, "migrationGeneration": migration.Generation,
		"migrationReferrers": migration.Referrers, "preMigrationReferrers": len(before.Manifests),
		"postMigrationReferrers": len(after.Manifests), "migrationState": migrationState.State,
		"fallbackDeleteStatus": deleteStatus, "registryServiceRestored": true, "restoredRegistryProbeStatus": restoredStatus,
	})
}

func referrerDescriptor(content []byte, digest, subject string) (support.Descriptor, error) {
	var manifest support.Manifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		return support.Descriptor{}, err
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != support.OCIManifestMediaType ||
		manifest.Subject == nil || manifest.Subject.Digest != subject || manifest.ArtifactType == "" {
		return support.Descriptor{}, fmt.Errorf("generated referrer manifest is invalid")
	}
	return support.Descriptor{
		MediaType: manifest.MediaType, Size: len(content), Digest: digest,
		ArtifactType: manifest.ArtifactType, Annotations: manifest.Annotations,
	}, nil
}

func publishLegacyIndex(ctx context.Context, registry *support.RegistryClient, repository, tag string, descriptor support.Descriptor) (string, error) {
	content, err := json.Marshal(support.Manifest{
		SchemaVersion: 2, MediaType: support.OCIIndexMediaType, Manifests: []support.Descriptor{descriptor},
	})
	if err != nil {
		return "", err
	}
	response, digest, err := registry.PutManifest(ctx, repository, tag, support.OCIIndexMediaType, content, nil)
	if response != nil {
		response.Body.Close()
	}
	if err != nil {
		return "", fmt.Errorf("publish legacy fallback index: %w", err)
	}
	return digest, nil
}

func readReferrers(ctx context.Context, registry *support.RegistryClient, repository, subject string) (support.Manifest, error) {
	response, err := registry.Referrers(ctx, repository, subject, nil)
	if err != nil {
		return support.Manifest{}, err
	}
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), support.OCIIndexMediaType) {
		response.Body.Close()
		return support.Manifest{}, fmt.Errorf("backend Referrers returned %s", response.Status)
	}
	var document support.Manifest
	if _, err := support.DecodeJSON(response, 4<<20, &document); err != nil {
		return support.Manifest{}, err
	}
	if document.SchemaVersion != 2 || document.MediaType != support.OCIIndexMediaType {
		return support.Manifest{}, fmt.Errorf("backend Referrers response is not an OCI index")
	}
	return document, nil
}

func readManifestIndex(ctx context.Context, registry *support.RegistryClient, repository, tag string) (support.Manifest, error) {
	response, err := registry.Get(ctx, repository, "/manifests/"+tag, map[string]string{"Accept": support.OCIIndexMediaType})
	if err != nil {
		return support.Manifest{}, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return support.Manifest{}, fmt.Errorf("fallback index GET returned %s", response.Status)
	}
	var document support.Manifest
	if _, err := support.DecodeJSON(response, 4<<20, &document); err != nil {
		return support.Manifest{}, err
	}
	if document.SchemaVersion != 2 || document.MediaType != support.OCIIndexMediaType {
		return support.Manifest{}, fmt.Errorf("fallback index body is invalid")
	}
	return document, nil
}

func requireDescriptors(descriptors []support.Descriptor, expected map[string]string) error {
	if len(descriptors) != len(expected) {
		return fmt.Errorf("found %d descriptors, want %d", len(descriptors), len(expected))
	}
	for _, descriptor := range descriptors {
		if expected[descriptor.Digest] != descriptor.ArtifactType {
			return fmt.Errorf("unexpected referrer descriptor %s with artifact type %q", descriptor.Digest, descriptor.ArtifactType)
		}
	}
	return nil
}

type migrationResponse struct {
	Action              string `json:"action"`
	Complete            bool   `json:"complete"`
	Generation          uint64 `json:"generation"`
	Referrers           int    `json:"referrers"`
	NativeIndexDigest   string `json:"nativeIndexDigest"`
	FallbackIndexDigest string `json:"fallbackIndexDigest"`
}
