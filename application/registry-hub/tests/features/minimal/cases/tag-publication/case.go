package tagpublication

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "tag-publication")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	registry := support.NewRegistryClient(environment)
	repository := support.Repository("tags", environment.RunID)
	first, err := support.BuildImageFixture(ctx, registry, repository, "winning", environment.RunID+"-first")
	if err != nil {
		return err
	}
	second, err := support.BuildImageFixture(ctx, registry, repository, "other", environment.RunID+"-second")
	if err != nil {
		return err
	}
	if err := putExistingManifest(ctx, registry, repository, "winning", first.ManifestType, first.Manifest); err != nil {
		return err
	}
	conflictResponse, _, conflictErr := registry.PutManifest(ctx, repository, "winning", second.ManifestType, second.Manifest, nil)
	if conflictResponse != nil {
		conflictResponse.Body.Close()
	}
	if conflictErr == nil || conflictResponse == nil || conflictResponse.StatusCode != http.StatusConflict {
		return fmt.Errorf("different digest tag update was not rejected with 409")
	}
	if raw, _, readErr := support.ReadManifest(ctx, registry, repository, "winning", first.ManifestType); readErr != nil || !bytes.Equal(raw, first.Manifest) {
		return fmt.Errorf("conflicting tag update changed the winning manifest")
	}

	query := url.Values{}
	query.Add("tag", "alias-a")
	query.Add("tag", "alias-a")
	query.Add("tag", "alias-b")
	response, _, err := registry.PutManifest(ctx, repository, first.ManifestDigest, first.ManifestType, first.Manifest, query)
	if response != nil {
		response.Body.Close()
	}
	if err != nil {
		return err
	}

	var winnerDigest string
	var winnerMu sync.Mutex
	var wait sync.WaitGroup
	wait.Add(2)
	for _, content := range [][]byte{first.Manifest, second.Manifest} {
		go func(value []byte) {
			defer wait.Done()
			result, _, putErr := registry.PutManifest(ctx, repository, "race", support.OCIManifestMediaType, value, nil)
			if result != nil {
				status := result.StatusCode
				result.Body.Close()
				winnerMu.Lock()
				if putErr == nil && status == http.StatusCreated {
					winnerDigest = support.Digest(value)
				}
				winnerMu.Unlock()
			}
		}(content)
	}
	wait.Wait()
	race, _, err := support.ReadManifest(ctx, registry, repository, "race", support.OCIManifestMediaType)
	if err != nil {
		return err
	}
	if winnerDigest == "" {
		return fmt.Errorf("concurrent tag publication had no successful winner")
	}
	if support.Digest(race) != winnerDigest {
		return fmt.Errorf("race winner was overwritten")
	}
	return support.WriteResult(environment, map[string]any{"repository": repository, "digest": first.ManifestDigest, "tags": []string{"winning", "other", "alias-a", "alias-b", "race"}, "raceDigest": winnerDigest})
}

func putExistingManifest(ctx context.Context, registry *support.RegistryClient, repository, reference, mediaType string, content []byte) error {
	for attempt := 0; attempt < 75; attempt++ {
		response, _, err := registry.PutManifest(ctx, repository, reference, mediaType, content, nil)
		if response != nil {
			status := response.StatusCode
			response.Body.Close()
			if err == nil {
				return nil
			}
			if status != http.StatusConflict || !strings.Contains(err.Error(), "tag publication is already in progress") {
				return err
			}
		} else if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("idempotent tag publication remained busy after retries")
}
