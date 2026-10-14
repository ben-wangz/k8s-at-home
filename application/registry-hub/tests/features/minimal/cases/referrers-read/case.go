package referrersread

import (
	"context"
	"embed"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "referrers-read")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	dependency := environment.DependencyResults["artifact-roundtrip"]
	repository, err := support.DetailString(dependency, "repository")
	if err != nil {
		return err
	}
	subject, err := support.DetailString(dependency, "subjectDigest")
	if err != nil {
		return err
	}
	registry := support.NewRegistryClient(environment)
	referrerDigests := make([]string, 0, 3)
	for index, artifactType := range []string{"application/vnd.example.provenance", "application/vnd.example.signature", "application/vnd.example.sbom"} {
		_, digest, buildErr := support.BuildReferrer(ctx, registry, repository, fmt.Sprintf("extra-referrer-%d", index), artifactType, subject)
		if buildErr != nil {
			return buildErr
		}
		referrerDigests = append(referrerDigests, digest)
	}
	response, err := registry.Referrers(ctx, repository, subject, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), support.OCIIndexMediaType) {
		response.Body.Close()
		return fmt.Errorf("native referrers endpoint returned %s", response.Status)
	}
	var document map[string]any
	if _, err := support.DecodeJSON(response, 4<<20, &document); err != nil {
		return err
	}
	descriptors := descriptorList(document["manifests"])
	if len(descriptors) < len(referrerDigests)+1 {
		return fmt.Errorf("native referrers result contains %d descriptors", len(descriptors))
	}
	for _, descriptor := range descriptors {
		if strings.Contains(fmt.Sprint(descriptor), "_state") || strings.Contains(fmt.Sprint(descriptor), "registry-hub-registry") {
			return fmt.Errorf("referrer descriptor leaked internal identity")
		}
	}
	filterArtifactType := "application/vnd.example.provenance"
	query := url.Values{"artifactType": {filterArtifactType}, "n": {"1"}}
	response, err = registry.Referrers(ctx, repository, subject, query)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return fmt.Errorf("filtered referrers returned %s", response.Status)
	}
	if response.Header.Get("OCI-Filters-Applied") != "artifactType" {
		response.Body.Close()
		return fmt.Errorf("filtered referrers did not report OCI-Filters-Applied")
	}
	link := response.Header.Get("Link")
	if link == "" {
		response.Body.Close()
		return fmt.Errorf("filtered referrers page has no next Link")
	}
	var filtered map[string]any
	if _, err := support.DecodeJSON(response, 4<<20, &filtered); err != nil {
		return err
	}
	filteredDescriptors := descriptorList(filtered["manifests"])
	if len(filteredDescriptors) != 1 {
		return fmt.Errorf("filtered page contains %d descriptors, want 1", len(filteredDescriptors))
	}
	firstDigest := testkit.String(filteredDescriptors[0]["digest"])
	for _, descriptor := range filteredDescriptors {
		if value := descriptor["artifactType"]; value != filterArtifactType {
			return fmt.Errorf("artifactType filter was not applied")
		}
	}
	next, err := nextLink(link)
	if err != nil {
		return err
	}
	next, err = testkit.ResolveLocation(registry.Endpoint(repository, "/referrers/"+subject), next)
	if err != nil {
		return err
	}
	nextResponse, err := registry.Request(ctx, http.MethodGet, next, nil, nil)
	if err != nil {
		return err
	}
	if nextResponse.StatusCode != http.StatusOK {
		nextResponse.Body.Close()
		return fmt.Errorf("referrer next page returned %s", nextResponse.Status)
	}
	var nextDocument map[string]any
	if _, err := support.DecodeJSON(nextResponse, 4<<20, &nextDocument); err != nil {
		return err
	}
	nextDescriptors := descriptorList(nextDocument["manifests"])
	if len(nextDescriptors) != 1 || nextDescriptors[0]["artifactType"] != filterArtifactType || testkit.String(nextDescriptors[0]["digest"]) == firstDigest {
		return fmt.Errorf("referrer next page did not contain the next filtered descriptor")
	}
	if nextResponse.Header.Get("Link") != "" {
		return fmt.Errorf("referrer next page unexpectedly has another Link")
	}
	tamperedURL, err := url.Parse(next)
	if err != nil {
		return err
	}
	tamperedQuery := tamperedURL.Query()
	cursor := tamperedQuery.Get("last")
	if cursor == "" {
		return fmt.Errorf("referrer next Link has no cursor")
	}
	tamperedQuery.Set("last", cursor+"x")
	tamperedURL.RawQuery = tamperedQuery.Encode()
	tamperedResponse, err := registry.Request(ctx, http.MethodGet, tamperedURL.String(), nil, nil)
	if err != nil {
		return err
	}
	if tamperedResponse.StatusCode != http.StatusConflict {
		tamperedResponse.Body.Close()
		return fmt.Errorf("tampered referrer cursor returned %s", tamperedResponse.Status)
	}
	tamperedResponse.Body.Close()

	emptyRepository := support.LocalRepository("referrers-empty", environment.RunID)
	emptySubject, err := support.BuildImageFixture(ctx, registry, emptyRepository, "subject", environment.RunID+"-empty-referrers")
	if err != nil {
		return err
	}
	emptyResponse, err := registry.Referrers(ctx, emptyRepository, emptySubject.ManifestDigest, nil)
	if err != nil {
		return err
	}
	if emptyResponse.StatusCode != http.StatusOK {
		emptyResponse.Body.Close()
		return fmt.Errorf("native empty referrers result returned %s", emptyResponse.Status)
	}
	var emptyDocument map[string]any
	if _, err := support.DecodeJSON(emptyResponse, 4<<20, &emptyDocument); err != nil {
		return err
	}
	emptyDescriptors := descriptorList(emptyDocument["manifests"])
	if len(emptyDescriptors) != 0 {
		return fmt.Errorf("native empty result unexpectedly contained %d referrers", len(emptyDescriptors))
	}
	return support.WriteResult(environment, map[string]any{"repository": repository, "subjectDigest": subject, "referrerDigests": referrerDigests, "descriptorCount": len(descriptors), "filterArtifactType": filterArtifactType, "filteredPageCount": len(filteredDescriptors), "nextPageCount": len(nextDescriptors), "tamperedCursorStatus": http.StatusConflict, "emptyRepository": emptyRepository, "emptySubjectDigest": emptySubject.ManifestDigest, "emptyDescriptorCount": len(emptyDescriptors)})
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

func nextLink(value string) (string, error) {
	start := strings.Index(value, "<")
	end := strings.Index(value, ">")
	if start < 0 || end <= start {
		return "", fmt.Errorf("invalid referrer Link")
	}
	return value[start+1 : end], nil
}
