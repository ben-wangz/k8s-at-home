package blobupload

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
	definition, raw, err := testkit.LoadDefinition(metadata, "blob-upload")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	registry := support.NewRegistryClient(environment)
	repository := support.LocalRepository("upload", environment.RunID)
	content := []byte("upload-fixture:" + environment.RunID)
	digest := support.Digest(content)
	response, location, err := registry.CreateUpload(ctx, repository, nil)
	if response == nil {
		return err
	}
	if response.StatusCode != http.StatusAccepted {
		response.Body.Close()
		return err
	}
	uploadUUID := response.Header.Get("Docker-Upload-UUID")
	response.Body.Close()
	parsedLocation, parseErr := url.Parse(location)
	if parseErr != nil {
		return fmt.Errorf("upload Location is invalid: %w", parseErr)
	}
	expectedPath := "/v2/" + repository + "/blobs/uploads/" + uploadUUID
	locationQuery := parsedLocation.Query()
	if uploadUUID == "" || parsedLocation.Path != expectedPath || len(locationQuery) != 1 || locationQuery.Get("_state") == "" {
		return fmt.Errorf("upload Location is not an opaque backend URL")
	}

	response, err = registry.Request(ctx, http.MethodGet, location, nil, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusNoContent {
		response.Body.Close()
		return fmt.Errorf("upload status returned %s", response.Status)
	}
	if updated := response.Header.Get("Location"); updated != "" {
		location, err = testkit.ResolveLocation(location, updated)
		if err != nil {
			response.Body.Close()
			return err
		}
	}
	response.Body.Close()

	first := content[:len(content)/2]
	second := content[len(content)/2:]
	response, next, err := registry.PatchUpload(ctx, location, first, 0)
	if response != nil {
		response.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("first upload patch failed: %w", err)
	}
	response, next, err = registry.PatchUpload(ctx, next, second, len(first))
	if response != nil {
		response.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("second upload patch failed: %w", err)
	}
	response, err = registry.Request(ctx, http.MethodGet, next, nil, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusNoContent {
		response.Body.Close()
		return fmt.Errorf("completed upload status returned %s", response.Status)
	}
	if updated := response.Header.Get("Location"); updated != "" {
		next, err = testkit.ResolveLocation(next, updated)
		if err != nil {
			response.Body.Close()
			return err
		}
	}
	response.Body.Close()
	response, err = registry.CommitUpload(ctx, next, digest)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusCreated {
		response.Body.Close()
		return fmt.Errorf("upload commit returned %s", response.Status)
	}
	response.Body.Close()
	uploaded, err := support.ReadBlob(ctx, registry, repository, digest)
	if err != nil || !bytes.Equal(uploaded, content) {
		return fmt.Errorf("committed blob did not match fixture: %w", err)
	}

	invalidContent := []byte("invalid-offset")
	offsetResponse, offsetLocation, offsetErr := registry.CreateUpload(ctx, repository, nil)
	if offsetResponse == nil {
		return offsetErr
	}
	if offsetErr != nil {
		offsetResponse.Body.Close()
		return offsetErr
	}
	offsetResponse.Body.Close()
	response, _, err = registry.PatchUpload(ctx, offsetLocation, invalidContent, 7)
	if response != nil {
		if response.StatusCode != http.StatusRequestedRangeNotSatisfiable {
			response.Body.Close()
			if err != nil {
				return fmt.Errorf("invalid offset returned %s: %w", response.Status, err)
			}
			return fmt.Errorf("invalid offset returned %s", response.Status)
		}
		response.Body.Close()
	}
	if err == nil {
		return fmt.Errorf("invalid offset unexpectedly succeeded")
	}
	cleanupOffset, cleanupErr := registry.CancelUpload(ctx, offsetLocation)
	if cleanupErr != nil {
		return cleanupErr
	}
	cleanupOffset.Body.Close()

	cancelResponse, cancelLocation, err := registry.CreateUpload(ctx, repository, nil)
	if err != nil {
		return err
	}
	cancelResponse.Body.Close()
	cancelResponse, err = registry.CancelUpload(ctx, cancelLocation)
	if err != nil {
		return err
	}
	if cancelResponse.StatusCode != http.StatusNoContent {
		cancelResponse.Body.Close()
		return fmt.Errorf("upload cancellation returned %s", cancelResponse.Status)
	}
	cancelResponse.Body.Close()
	cancelResponse, err = registry.Request(ctx, http.MethodGet, cancelLocation, nil, nil)
	if err != nil {
		return err
	}
	if cancelResponse.StatusCode != http.StatusNotFound {
		cancelResponse.Body.Close()
		return fmt.Errorf("cancelled upload returned %s", cancelResponse.Status)
	}
	cancelResponse.Body.Close()

	monolithic := []byte("monolithic:" + environment.RunID)
	monolithicDigest := support.Digest(monolithic)
	query := url.Values{}
	query.Add("digest", monolithicDigest)
	response, err = registry.Request(ctx, http.MethodPost, registry.Endpoint(repository, "/blobs/uploads/")+"?"+query.Encode(), bytes.NewReader(monolithic), map[string]string{"Content-Type": "application/octet-stream"})
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusCreated {
		response.Body.Close()
	} else if response.StatusCode == http.StatusAccepted {
		location := response.Header.Get("Location")
		response.Body.Close()
		if location == "" {
			return fmt.Errorf("monolithic fallback returned no Location")
		}
		resolved, resolveErr := testkit.ResolveLocation(registry.Endpoint(repository, "/blobs/uploads/"), location)
		if resolveErr != nil {
			return resolveErr
		}
		patch, next, patchErr := registry.PatchUpload(ctx, resolved, monolithic, 0)
		if patch != nil {
			patch.Body.Close()
		}
		if patchErr != nil {
			return fmt.Errorf("monolithic fallback patch failed: %w", patchErr)
		}
		commit, commitErr := registry.CommitUpload(ctx, next, monolithicDigest)
		if commitErr != nil {
			return commitErr
		}
		if commit.StatusCode != http.StatusCreated {
			commit.Body.Close()
			return fmt.Errorf("monolithic fallback commit returned %s", commit.Status)
		}
		commit.Body.Close()
	} else {
		defer response.Body.Close()
		return fmt.Errorf("monolithic upload returned %s", response.Status)
	}
	return support.WriteResult(environment, map[string]any{
		"repository": repository,
		"digest":     digest,
		"uploadUUID": uploadUUID,
		"locationHost": func() string {
			parsed, _ := url.Parse(location)
			if parsed == nil {
				return ""
			}
			return parsed.Host
		}(),
	})
}
