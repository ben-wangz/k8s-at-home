package clientauthcatalog

import (
	"bufio"
	"context"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"strconv"
	"strings"
)

func readCatalogPages(ctx context.Context, registry *support.RegistryClient) ([]string, int, error) {
	endpoint := registry.Base + "/v2/_catalog?n=1"
	visited := make(map[string]struct{})
	names := make([]string, 0, 3)
	for page := 0; page < 10; page++ {
		if _, exists := visited[endpoint]; exists {
			return nil, page, fmt.Errorf("catalog pagination repeated URL %s", endpoint)
		}
		visited[endpoint] = struct{}{}
		response, err := registry.Request(ctx, http.MethodGet, endpoint, nil, nil)
		if err != nil {
			return nil, page, err
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return nil, page, fmt.Errorf("catalog page %d returned %s", page+1, response.Status)
		}
		link := response.Header.Get("Link")
		var result struct {
			Repositories []string `json:"repositories"`
		}
		if _, err := support.DecodeJSON(response, 2<<20, &result); err != nil {
			return nil, page, fmt.Errorf("decode catalog page %d: %w", page+1, err)
		}
		if len(result.Repositories) > 1 {
			return nil, page, fmt.Errorf("catalog page %d contains more than one repository", page+1)
		}
		names = append(names, result.Repositories...)
		if link == "" {
			return names, page + 1, nil
		}
		if !strings.Contains(link, `rel="next"`) {
			return nil, page + 1, fmt.Errorf("catalog Link is not a next-page link")
		}
		start := strings.IndexByte(link, '<')
		end := strings.IndexByte(link, '>')
		if start < 0 || end <= start {
			return nil, page + 1, fmt.Errorf("catalog next-page Link is malformed")
		}
		next := link[start+1 : end]
		if !strings.HasPrefix(next, "/v2/_catalog?") || strings.Contains(next, "_state") {
			return nil, page + 1, fmt.Errorf("catalog next-page Link exposes an internal path or cursor")
		}
		endpoint, err = testkit.ResolveLocation(endpoint, next)
		if err != nil {
			return nil, page + 1, fmt.Errorf("resolve catalog next-page Link: %w", err)
		}
	}
	return nil, 10, fmt.Errorf("catalog pagination exceeded the page limit")
}

func upstreamRequestTotal(ctx context.Context, environment testkit.CaseContext) (float64, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/metrics", nil, nil)
	if err != nil {
		return 0, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return 0, fmt.Errorf("metrics returned %s", response.Status)
	}
	body, err := testkit.ReadBody(response, 4<<20)
	if err != nil {
		return 0, err
	}
	var total float64
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "registry_hub_upstream_requests_total{") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return 0, fmt.Errorf("upstream metric sample is malformed")
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return 0, fmt.Errorf("parse upstream metric sample: %w", err)
		}
		total += value
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return total, nil
}

func validateAuthFixture(auth support.AdminAuth) error {
	if len(auth.Basic) != 3 || len(auth.Bearer) != 1 {
		return fmt.Errorf("client auth fixture must contain three Basic and one Bearer credential")
	}
	if auth.Basic[0].Username != "reader" || len(auth.Basic[0].Password) < 32 || !equal(auth.Basic[0].Permissions, []string{"pull"}) {
		return fmt.Errorf("client pull credential is invalid")
	}
	if auth.Basic[1].Username != "catalog-reader" || len(auth.Basic[1].Password) < 32 || !equal(auth.Basic[1].Permissions, []string{"catalog"}) {
		return fmt.Errorf("client catalog credential is invalid")
	}
	if auth.Basic[2].Username != "writer" || len(auth.Basic[2].Password) < 32 || !equal(auth.Basic[2].Permissions, []string{"pull", "push", "delete"}) {
		return fmt.Errorf("client writer credential is invalid")
	}
	if auth.Bearer[0].Username != "bearer-reader" || len(auth.Bearer[0].Token) < 32 || !equal(auth.Bearer[0].Permissions, []string{"pull"}) {
		return fmt.Errorf("client Bearer credential is invalid")
	}
	return nil
}

func equal(left, right []string) bool {
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
