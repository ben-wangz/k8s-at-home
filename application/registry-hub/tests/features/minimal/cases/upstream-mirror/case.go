package upstreammirror

import (
	"context"
	"embed"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"net/url"
	"strings"
)

//go:embed case.json
var metadata embed.FS

const publicAuthorityIP = "34.192.215.158"

const (
	dockerListBody = `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.list.v2+json","manifests":[]}`
)

type upstreamCredential struct {
	Type     string `json:"type"`
	Username string `json:"username"`
	Password string `json:"password"`
	Token    string `json:"token"`
}

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "upstream-mirror")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func verifyUpstreamTagPagination(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient) (map[string]any, error) {
	const reference = "minimal-upstream-tags"
	const tagsRepository = "registry-hub-test/tags-only"
	externalRepository := "github.com/" + tagsRepository
	localFixture, err := support.BuildImageFixture(ctx, registry, externalRepository, "local-only", environment.RunID)
	if err != nil {
		return nil, fmt.Errorf("create local tags fixture: %w", err)
	}
	fixtureRequestsBeforeLocal, err := countFixturePath(ctx, environment, "/v2/registry-hub-test/tags-only/tags/list")
	if err != nil {
		return nil, err
	}
	local, err := registry.Tags(ctx, externalRepository, url.Values{"n": {"2"}})
	if err != nil {
		return nil, err
	}
	if local.StatusCode != http.StatusOK {
		local.Body.Close()
		return nil, fmt.Errorf("local tags returned %s", local.Status)
	}
	var localPage map[string]any
	if _, err := support.DecodeJSON(local, 1<<20, &localPage); err != nil {
		return nil, err
	}
	if testkit.String(localPage["name"]) != externalRepository || !equalStrings(stringValues(localPage["tags"]), []string{localFixture.Tag}) {
		return nil, fmt.Errorf("local tags response was merged with upstream tags or changed its external name")
	}
	fixtureRequestsAfterLocal, err := countFixturePath(ctx, environment, "/v2/registry-hub-test/tags-only/tags/list")
	if err != nil {
		return nil, err
	}
	if fixtureRequestsAfterLocal != fixtureRequestsBeforeLocal {
		return nil, fmt.Errorf("local tags hit reached upstream fixture: requests changed from %d to %d", fixtureRequestsBeforeLocal, fixtureRequestsAfterLocal)
	}
	beforeTasks, err := countTasksForReference(ctx, environment, "github.com", tagsRepository, reference)
	if err != nil {
		return nil, err
	}
	first, err := registry.Tags(ctx, externalRepository, url.Values{"n": {"2"}, "_origin": {"upstream"}})
	if err != nil {
		return nil, err
	}
	if first.StatusCode != http.StatusOK {
		first.Body.Close()
		return nil, fmt.Errorf("upstream tags first page returned %s", first.Status)
	}
	var firstPage map[string]any
	if _, err := support.DecodeJSON(first, 1<<20, &firstPage); err != nil {
		return nil, err
	}
	if testkit.String(firstPage["name"]) != externalRepository || !equalStrings(stringValues(firstPage["tags"]), []string{"alpha", "beta"}) {
		return nil, fmt.Errorf("upstream tags first page has unexpected name or tags")
	}
	link := first.Header.Get("Link")
	start, end := strings.IndexByte(link, '<'), strings.IndexByte(link, '>')
	if start < 0 || end <= start {
		return nil, fmt.Errorf("upstream tags first page has no valid next Link")
	}
	next, err := url.Parse(link[start+1 : end])
	expectedEndpoint, endpointErr := url.Parse(registry.Endpoint(externalRepository, "/tags/list"))
	if err != nil || endpointErr != nil || next.IsAbs() || next.Path != expectedEndpoint.Path {
		return nil, fmt.Errorf("upstream tags Link does not use the external backend repository path")
	}
	if next.Query().Get("last") != "beta" || next.Query().Get("n") != "2" || next.Query().Get("_origin") != "upstream" {
		return nil, fmt.Errorf("upstream tags Link lost its cursor, page size or source")
	}
	second, err := registry.Request(ctx, http.MethodGet, registry.Base+next.RequestURI(), nil, nil)
	if err != nil {
		return nil, err
	}
	if second.StatusCode != http.StatusOK {
		second.Body.Close()
		return nil, fmt.Errorf("upstream tags second page returned %s", second.Status)
	}
	var secondPage map[string]any
	if _, err := support.DecodeJSON(second, 1<<20, &secondPage); err != nil {
		return nil, err
	}
	if testkit.String(secondPage["name"]) != externalRepository || !equalStrings(stringValues(secondPage["tags"]), []string{"gamma"}) || second.Header.Get("Link") != "" {
		return nil, fmt.Errorf("upstream tags second page has unexpected name, tags or next Link")
	}
	requests, err := countFixturePath(ctx, environment, "/v2/registry-hub-test/tags-only/tags/list")
	if err != nil {
		return nil, err
	}
	afterTasks, err := countTasksForReference(ctx, environment, "github.com", tagsRepository, reference)
	if err != nil {
		return nil, err
	}
	if requests-fixtureRequestsAfterLocal != 2 || beforeTasks != afterTasks || afterTasks != 0 {
		return nil, fmt.Errorf("upstream tags issued %d fixture requests and changed matching tasks from %d to %d", requests-fixtureRequestsAfterLocal, beforeTasks, afterTasks)
	}
	alternateAuthority, err := verifyAlternateAuthorityTagList(ctx, environment, registry)
	if err != nil {
		return nil, err
	}
	firstTags := stringValues(firstPage["tags"])
	secondTags := stringValues(secondPage["tags"])
	return map[string]any{
		"pages": 2, "tags": []string{"alpha", "beta", "gamma"},
		"externalName": testkit.String(firstPage["name"]), "externalLink": link,
		"firstPageTags": firstTags, "secondPageTags": secondTags,
		"localTags": stringValues(localPage["tags"]), "fixtureRequests": requests - fixtureRequestsAfterLocal, "solidificationTasks": afterTasks,
		"alternateAuthority": alternateAuthority,
	}, nil
}

func verifyAlternateAuthorityTagList(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient) (map[string]any, error) {
	const authority = "quay.io"
	const repository = "registry-hub-test/tags-only"
	externalRepository := authority + "/" + repository
	beforeTasks, err := countTasksForReference(ctx, environment, authority, repository, "minimal-upstream-tags")
	if err != nil {
		return nil, err
	}
	response, err := registry.Tags(ctx, externalRepository, url.Values{"_origin": {"upstream"}})
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("alternate authority tags returned %s", response.Status)
	}
	var page map[string]any
	if _, err := support.DecodeJSON(response, 1<<20, &page); err != nil {
		return nil, err
	}
	if testkit.String(page["name"]) != externalRepository || !equalStrings(stringValues(page["tags"]), []string{"alpha", "beta", "gamma"}) {
		return nil, fmt.Errorf("alternate authority tags response lost its external name or values")
	}
	requests, err := countFixturePathForHost(ctx, environment, authority, "/v2/"+repository+"/tags/list")
	if err != nil {
		return nil, err
	}
	afterTasks, err := countTasksForReference(ctx, environment, authority, repository, "minimal-upstream-tags")
	if err != nil {
		return nil, err
	}
	if requests != 1 || beforeTasks != afterTasks || afterTasks != 0 {
		return nil, fmt.Errorf("alternate authority made %d fixture requests and changed task count from %d to %d", requests, beforeTasks, afterTasks)
	}
	return map[string]any{"externalName": testkit.String(page["name"]), "fixtureRequests": requests, "solidificationTasks": afterTasks}, nil
}

func stringValues(value any) []string {
	items := testkit.Slice(value)
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
