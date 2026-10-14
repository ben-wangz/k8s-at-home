package registryprotocol

import (
	"context"
	"embed"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "registry-protocol")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	registry := support.NewRegistryClient(environment)
	response, err := registry.Version(ctx)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Docker-Distribution-API-Version") != "registry/2.0" {
		response.Body.Close()
		return fmt.Errorf("Registry version response is not standard: %s", response.Status)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		response.Body.Close()
		return err
	}
	response.Body.Close()

	repository := support.LocalRepository("protocol", environment.RunID)
	response, err = registry.Get(ctx, repository, "/manifests/missing", nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusNotFound {
		response.Body.Close()
		return fmt.Errorf("local missing manifest returned %s", response.Status)
	}
	if _, err := support.DecodeError(response); err != nil {
		return fmt.Errorf("local error envelope: %w", err)
	}

	response, err = registry.Head(ctx, repository, "/blobs/"+support.Digest([]byte("missing")), nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusNotFound {
		response.Body.Close()
		return fmt.Errorf("missing blob HEAD returned %s", response.Status)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return err
	}
	if len(body) != 0 {
		return fmt.Errorf("HEAD returned a response body")
	}

	response, err = registry.Get(ctx, repository, "/_catalog", nil)
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusOK {
		response.Body.Close()
		return fmt.Errorf("catalog is enabled in the minimal configuration")
	}
	response.Body.Close()

	response, err = registry.Request(ctx, http.MethodPost, registry.Endpoint(repository, "/tags/list"), nil, nil)
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusOK {
		response.Body.Close()
		return fmt.Errorf("POST tags/list was accepted")
	}
	response.Body.Close()

	response, err = testkit.Request(ctx, registry.Client, http.MethodGet, strings.TrimRight(environment.AdminURL, "/")+"/v2/", nil, nil, nil)
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusOK {
		response.Body.Close()
		return fmt.Errorf("admin listener exposed Registry API")
	}
	response.Body.Close()
	response, err = registry.Request(ctx, http.MethodGet, registry.Base+"/api/v1/tasks", nil, nil)
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusOK {
		response.Body.Close()
		return fmt.Errorf("client listener exposed management API")
	}
	response.Body.Close()

	return support.WriteResult(environment, map[string]any{
		"repository":    repository,
		"version":       "verified",
		"errorEnvelope": true,
		"headBodyBytes": 0,
	})
}
