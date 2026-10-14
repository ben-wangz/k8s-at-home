package blobmount

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"net/http"
	"net/url"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "blob-mount")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	dependency := environment.DependencyResults["blob-upload"]
	sourceRepository, err := support.DetailString(dependency, "repository")
	if err != nil {
		return err
	}
	digest, err := support.DetailString(dependency, "digest")
	if err != nil {
		return err
	}
	registry := support.NewRegistryClient(environment)
	source, err := support.ReadBlob(ctx, registry, sourceRepository, digest)
	if err != nil {
		return err
	}
	targetRepository := support.Repository("mount", environment.RunID)
	query := url.Values{}
	query.Set("mount", digest)
	query.Set("from", sourceRepository)
	endpoint := registry.Endpoint(targetRepository, "/blobs/uploads/") + "?" + query.Encode()
	response, err := registry.Request(ctx, http.MethodPost, endpoint, nil, nil)
	if err != nil {
		return err
	}
	switch response.StatusCode {
	case http.StatusCreated:
		response.Body.Close()
	case http.StatusAccepted:
		location := response.Header.Get("Location")
		response.Body.Close()
		if location == "" {
			return fmt.Errorf("mount fallback returned no Location")
		}
		upload, err := testkit.ResolveLocation(registry.Endpoint(targetRepository, "/blobs/uploads/"), location)
		if err != nil {
			return err
		}
		commit, err := registry.CommitUpload(ctx, upload, digest)
		if err != nil {
			return err
		}
		if commit.StatusCode != http.StatusCreated {
			commit.Body.Close()
			return fmt.Errorf("mount fallback commit returned %s", commit.Status)
		}
		commit.Body.Close()
	default:
		response.Body.Close()
		return fmt.Errorf("mount returned unsupported status %s", response.Status)
	}
	received, err := support.ReadBlob(ctx, registry, targetRepository, digest)
	if err != nil {
		return err
	}
	if !bytes.Equal(received, source) {
		return fmt.Errorf("mounted blob differs from source")
	}
	return support.WriteResult(environment, map[string]any{"sourceRepository": sourceRepository, "targetRepository": targetRepository, "digest": digest, "bytes": len(source)})
}
