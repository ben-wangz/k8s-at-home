package clientauthcatalog

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"sort"
	"strings"
	"time"
)

//go:embed case.json
var metadata embed.FS

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "feature-client-auth-catalog")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	secret, data, err := support.GetSecret(ctx, environment.Namespace, environment.Release+"-external-client-auth")
	if err != nil {
		return err
	}
	if secret.Labels["registry-hub.io/test-id"] != environment.RunID ||
		secret.Labels["registry-hub.io/instance"] != environment.Fullname ||
		secret.Labels["registry-hub.io/managed-by"] != "registry-hub-test-fixture" {
		return fmt.Errorf("client auth Secret ownership is invalid")
	}
	if len(data) != 1 || len(data["config.json"]) == 0 {
		return fmt.Errorf("client auth Secret must contain config.json only")
	}
	var auth support.AdminAuth
	if err := json.Unmarshal(data["config.json"], &auth); err != nil {
		return fmt.Errorf("client auth configuration is invalid")
	}
	if err := validateAuthFixture(auth); err != nil {
		return err
	}

	registry := support.NewRegistryClient(environment)
	response, err := registry.Version(ctx)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusUnauthorized || !strings.Contains(response.Header.Get("WWW-Authenticate"), "Basic") || !strings.Contains(response.Header.Get("WWW-Authenticate"), "Bearer") {
		response.Body.Close()
		return fmt.Errorf("client Registry did not challenge unauthenticated requests")
	}
	response.Body.Close()
	registry.Basic = &testkit.BasicCredential{Username: "writer", Password: auth.Basic[2].Password}
	response, err = registry.Version(ctx)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return fmt.Errorf("valid writer credentials returned %s", response.Status)
	}
	response.Body.Close()
	registry.Basic.Password = "invalid-registry-hub-test-password"
	response, err = registry.Version(ctx)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusUnauthorized {
		response.Body.Close()
		return fmt.Errorf("invalid client credentials returned %s", response.Status)
	}
	response.Body.Close()

	registry.Basic = &testkit.BasicCredential{Username: "writer", Password: auth.Basic[2].Password}
	repository := support.LocalRepository("client-auth", environment.RunID)
	fixture, err := support.BuildImageFixture(ctx, registry, repository, "published", environment.RunID+"-client-auth")
	if err != nil {
		return err
	}
	mappedRepository := "github.com/registry-hub-test/catalog-" + environment.RunID
	if _, err := support.BuildImageFixture(ctx, registry, mappedRepository, "mapped", environment.RunID+"-catalog-mapped"); err != nil {
		return fmt.Errorf("publish reverse-mapped catalog fixture: %w", err)
	}
	secondaryRepository := support.LocalRepository("catalog-secondary", environment.RunID)
	if _, err := support.BuildImageFixture(ctx, registry, secondaryRepository, "secondary", environment.RunID+"-catalog-secondary"); err != nil {
		return fmt.Errorf("publish secondary catalog fixture: %w", err)
	}
	reader := support.NewRegistryClient(environment)
	reader.Basic = &testkit.BasicCredential{Username: "reader", Password: auth.Basic[0].Password}
	manifest, _, err := support.ReadManifest(ctx, reader, repository, "published", support.OCIManifestMediaType)
	if err != nil || string(manifest) != string(fixture.Manifest) {
		return fmt.Errorf("pull-only client could not read the published manifest: %v", err)
	}
	response, err = reader.Request(ctx, http.MethodPost, reader.Endpoint(repository, "/blobs/uploads/"), nil, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusForbidden {
		response.Body.Close()
		return fmt.Errorf("pull-only client upload returned %s", response.Status)
	}
	response.Body.Close()
	response, err = reader.Request(ctx, http.MethodDelete, reader.Endpoint(repository, "/manifests/"+fixture.ManifestDigest), nil, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusForbidden {
		response.Body.Close()
		return fmt.Errorf("pull-only client manifest delete returned %s", response.Status)
	}
	response.Body.Close()

	bearer := support.NewRegistryClient(environment)
	bearer.Authorization = "Bearer " + auth.Bearer[0].Token
	manifest, _, err = support.ReadManifest(ctx, bearer, repository, "published", support.OCIManifestMediaType)
	if err != nil || string(manifest) != string(fixture.Manifest) {
		return fmt.Errorf("Bearer pull client could not read the published manifest: %v", err)
	}
	catalogReader := support.NewRegistryClient(environment)
	catalogReader.Basic = &testkit.BasicCredential{Username: "catalog-reader", Password: auth.Basic[1].Password}
	manifestResponse, err := catalogReader.Get(ctx, repository, "/manifests/published", nil)
	if err != nil {
		return err
	}
	if manifestResponse.StatusCode != http.StatusForbidden {
		manifestResponse.Body.Close()
		return fmt.Errorf("catalog-only client content pull returned %s", manifestResponse.Status)
	}
	manifestResponse.Body.Close()
	readerCatalog, err := reader.Request(ctx, http.MethodGet, reader.Base+"/v2/_catalog?n=10", nil, nil)
	if err != nil {
		return err
	}
	if readerCatalog.StatusCode != http.StatusForbidden {
		readerCatalog.Body.Close()
		return fmt.Errorf("pull-only client catalog returned %s", readerCatalog.Status)
	}
	readerCatalog.Body.Close()
	upstreamBefore, err := upstreamRequestTotal(ctx, environment)
	if err != nil {
		return err
	}
	names, catalogPages, err := readCatalogPages(ctx, catalogReader)
	if err != nil {
		return err
	}
	upstreamAfter, err := upstreamRequestTotal(ctx, environment)
	if err != nil {
		return err
	}
	expectedNames := []string{repository, mappedRepository, secondaryRepository}
	sort.Strings(names)
	sort.Strings(expectedNames)
	if len(names) != len(expectedNames) {
		return fmt.Errorf("catalog returned %d repositories, want %d", len(names), len(expectedNames))
	}
	for index := range names {
		if names[index] != expectedNames[index] {
			return fmt.Errorf("catalog repository %q at position %d, want %q", names[index], index, expectedNames[index])
		}
	}
	if catalogPages < 2 {
		return fmt.Errorf("catalog returned %d page(s) for three repositories", catalogPages)
	}
	if upstreamAfter != upstreamBefore {
		return fmt.Errorf("catalog changed upstream request count from %g to %g", upstreamBefore, upstreamAfter)
	}
	response, err = registry.Request(ctx, http.MethodDelete, registry.Endpoint(repository, "/manifests/"+fixture.ManifestDigest), nil, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusAccepted {
		response.Body.Close()
		return fmt.Errorf("writer manifest delete returned %s", response.Status)
	}
	response.Body.Close()
	response, err = registry.Get(ctx, repository, "/manifests/published", nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusNotFound {
		response.Body.Close()
		return fmt.Errorf("deleted writer manifest returned %s", response.Status)
	}
	response.Body.Close()
	unauthenticatedAdmin, err := testkit.Request(ctx, testkit.NewHTTPClient(10*time.Second, nil), http.MethodGet, environment.AdminURL+"/metrics", nil, nil, reader.Basic)
	if err != nil {
		return err
	}
	if unauthenticatedAdmin.StatusCode != http.StatusUnauthorized {
		unauthenticatedAdmin.Body.Close()
		return fmt.Errorf("client Registry credentials were accepted by the admin listener")
	}
	unauthenticatedAdmin.Body.Close()

	return support.WriteResult(environment, map[string]any{
		"secretUID":            secret.UID,
		"secretKeySHA256":      support.SecretHashes(data),
		"repository":           repository,
		"manifestDigest":       fixture.ManifestDigest,
		"catalogCount":         len(names),
		"catalogPages":         catalogPages,
		"catalogMappedName":    mappedRepository,
		"catalogUpstreamDelta": upstreamAfter - upstreamBefore,
		"permissionBoundary":   "pull-only client denied push and delete; writer delete succeeded",
		"authDomainIsolation":  "client credentials rejected by admin listener",
	})
}
