package routingpaths

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
	definition, raw, err := testkit.LoadDefinition(metadata, "routing-paths")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	registry := support.NewRegistryClient(environment)
	repositories := []string{
		"docker.io/library/" + environment.RunID,
		"library/" + environment.RunID,
		"host:5000/team/" + environment.RunID,
		"[2001:db8::1]:5000/team/" + environment.RunID,
		"localhost/" + environment.RunID,
	}
	digests := make(map[string]string, len(repositories))
	for index, repository := range repositories {
		fixture, err := support.BuildImageFixture(ctx, registry, repository, "route", fmt.Sprintf("%s-%d", environment.RunID, index))
		if err != nil {
			return fmt.Errorf("route fixture %s: %w", repository, err)
		}
		body, _, err := support.ReadManifest(ctx, registry, repository, "route", support.OCIManifestMediaType)
		if err != nil || string(body) != string(fixture.Manifest) {
			return fmt.Errorf("route fixture %s did not round-trip", repository)
		}
		digests[repository] = fixture.ManifestDigest
	}
	if len(digests) != len(repositories) {
		return fmt.Errorf("route fixtures collided")
	}

	local := support.LocalRepository("routing", environment.RunID)
	response, err := registry.Get(ctx, local, "/manifests/missing", nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusNotFound {
		response.Body.Close()
		return fmt.Errorf("single component repository miss returned %s", response.Status)
	}
	response.Body.Close()

	invalidPaths := []string{
		registry.Base + "/v2/docker.io/library/" + environment.RunID + "%2Fbad/manifests/x",
		registry.Base + "/v2/docker.io/library/" + environment.RunID + "/manifests/../bad",
		registry.Base + "/v2/docker.io/library//" + environment.RunID + "/manifests/x",
		registry.Base + "/v2/user%40info/team/image/manifests/x",
	}
	for _, endpoint := range invalidPaths {
		response, err := registry.Request(ctx, http.MethodGet, endpoint, nil, nil)
		if err != nil {
			continue
		}
		if response.StatusCode == http.StatusOK {
			response.Body.Close()
			return fmt.Errorf("invalid path was accepted: %s", endpoint)
		}
		response.Body.Close()
	}
	return support.WriteResult(environment, map[string]any{
		"repositories":     repositories,
		"manifestDigests":  digests,
		"invalidPathCount": len(invalidPaths),
	})
}
