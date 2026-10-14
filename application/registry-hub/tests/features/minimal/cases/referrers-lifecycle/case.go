package referrerslifecycle

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
	definition, raw, err := testkit.LoadDefinition(metadata, "referrers-lifecycle")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	dependency := environment.DependencyResults["referrers-read"]
	repository, err := support.DetailString(dependency, "repository")
	if err != nil {
		return err
	}
	subject, err := support.DetailString(dependency, "subjectDigest")
	if err != nil {
		return err
	}
	referrers := stringSlice(dependency.Details["referrerDigests"])
	if len(referrers) == 0 {
		return fmt.Errorf("referrers-read produced no referrer digests")
	}
	registry := support.NewRegistryClient(environment)
	deleted := referrers[0]
	response, err := registry.Delete(ctx, repository, "/manifests/"+deleted)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusAccepted {
		response.Body.Close()
		return fmt.Errorf("referrer delete returned %s", response.Status)
	}
	response.Body.Close()
	response, err = registry.Referrers(ctx, repository, subject, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return fmt.Errorf("referrers after delete returned %s", response.Status)
	}
	var document map[string]any
	if _, err := support.DecodeJSON(response, 4<<20, &document); err != nil {
		return err
	}
	for _, descriptor := range descriptorList(document["manifests"]) {
		if descriptor["digest"] == deleted {
			return fmt.Errorf("deleted referrer remains in native result")
		}
	}
	if _, _, err := support.ReadManifest(ctx, registry, repository, subject, support.OCIManifestMediaType); err != nil {
		return fmt.Errorf("subject was affected by referrer deletion: %w", err)
	}
	return support.WriteResult(environment, map[string]any{"repository": repository, "subjectDigest": subject, "deletedDigest": deleted, "remaining": len(descriptorList(document["manifests"])), "fallbackProtection": "not-applicable-native"})
}

func descriptorList(value any) []map[string]any {
	raw, _ := value.([]any)
	result := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if object, ok := item.(map[string]any); ok {
			result = append(result, object)
		}
	}
	return result
}

func stringSlice(value any) []string {
	raw, _ := value.([]any)
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}
