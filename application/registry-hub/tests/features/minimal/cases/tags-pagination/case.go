package tagspagination

import (
	"context"
	"embed"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "tags-pagination")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	registry := support.NewRegistryClient(environment)
	repository, err := support.DetailString(environment.DependencyResults["tag-publication"], "repository")
	if err != nil {
		return err
	}
	expected := []string{"winning", "other", "alias-a", "alias-b", "race"}
	sort.Strings(expected)
	response, err := registry.Tags(ctx, repository, url.Values{"n": {"0"}})
	if err != nil {
		return err
	}
	var empty map[string]any
	if _, err := support.DecodeJSON(response, 1<<20, &empty); err != nil {
		return err
	}
	if len(stringSlice(empty["tags"])) != 0 || response.Header.Get("Link") != "" || stringValue(empty["name"]) != repository {
		return fmt.Errorf("n=0 tags response is invalid")
	}

	all := make([]string, 0, len(expected))
	last := ""
	for page := 0; page < 10; page++ {
		query := url.Values{"n": {"2"}}
		if last != "" {
			query.Set("last", last)
		}
		response, err = registry.Tags(ctx, repository, query)
		if err != nil {
			return err
		}
		var document map[string]any
		if _, err := support.DecodeJSON(response, 1<<20, &document); err != nil {
			return err
		}
		if stringValue(document["name"]) != repository || strings.Contains(response.Header.Get("Link"), "_state") {
			return fmt.Errorf("tags response leaked internal information")
		}
		tags := stringSlice(document["tags"])
		all = append(all, tags...)
		if len(tags) == 0 || response.Header.Get("Link") == "" {
			break
		}
		last = tags[len(tags)-1]
	}
	if len(all) != len(expected) {
		return fmt.Errorf("paged tags count %d does not match %d", len(all), len(expected))
	}
	sort.Strings(all)
	for index := range all {
		if all[index] != expected[index] {
			return fmt.Errorf("paged tags differ at %d: %s != %s", index, all[index], expected[index])
		}
	}
	response, err = registry.Tags(ctx, repository, url.Values{"n": {"-1"}})
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusOK {
		response.Body.Close()
		return fmt.Errorf("negative page size was accepted")
	}
	response.Body.Close()
	return support.WriteResult(environment, map[string]any{"repository": repository, "tags": expected, "pages": (len(all) + 1) / 2})
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func stringSlice(value any) []string {
	raw, _ := value.([]any)
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		if value, ok := item.(string); ok {
			result = append(result, value)
		}
	}
	return result
}
