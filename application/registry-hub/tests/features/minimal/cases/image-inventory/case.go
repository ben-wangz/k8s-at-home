package imageinventory

import (
	"context"
	"embed"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "image-inventory")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	repository, err := support.DetailString(environment.DependencyResults["registry-roundtrip"], "repository")
	if err != nil {
		return err
	}
	authority, remote, _ := strings.Cut(repository, "/")
	client := testkit.NewHTTPClient(20*time.Second, nil)
	response, err := testkit.Request(ctx, client, http.MethodGet, environment.AdminURL+"/api/v1/images", nil, nil, nil)
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("inventory unauthenticated status=%d", response.StatusCode)
	}
	query := url.Values{"authority": {authority}, "repository": {remote}, "reference": {"minimal"}, "limit": {"1"}}
	var page map[string]any
	var image map[string]any
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		page, err = read(ctx, environment, "/api/v1/images?"+query.Encode())
		if err != nil {
			return err
		}
		items, _ := page["items"].([]any)
		if len(items) == 1 {
			image = testkit.Map(items[0])
			if testkit.String(image["category"]) == "solidified" {
				break
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if image == nil || testkit.String(image["category"]) != "solidified" {
		return fmt.Errorf("pushed image not discovered as solidified: %v", image)
	}
	id, snapshot := testkit.String(image["id"]), testkit.String(page["snapshotID"])
	if id == "" || snapshot == "" || testkit.String(testkit.Map(image["availability"])["state"]) != "complete" {
		return fmt.Errorf("incomplete inventory identity or availability")
	}
	for _, path := range []string{"/api/v1/images/" + id, "/api/v1/images/" + id + "/platforms", "/api/v1/images/summary", "/api/v1/cache/objects"} {
		result, err := read(ctx, environment, path+"?snapshotID="+snapshot)
		if err != nil {
			return err
		}
		if testkit.String(result["snapshotID"]) != snapshot {
			return fmt.Errorf("snapshot changed for %s", path)
		}
	}
	objects, err := read(ctx, environment, "/api/v1/images/"+id+"/objects?snapshotID="+snapshot+"&limit=1")
	if err != nil {
		return err
	}
	count := 0
	for {
		items, _ := objects["items"].([]any)
		count += len(items)
		token := testkit.String(objects["nextToken"])
		if token == "" {
			break
		}
		objects, err = read(ctx, environment, "/api/v1/images/"+id+"/objects?limit=1&token="+url.QueryEscape(token))
		if err != nil {
			return err
		}
		if testkit.String(objects["snapshotID"]) != snapshot {
			return fmt.Errorf("object pagination switched snapshot")
		}
	}
	if count != 3 {
		return fmt.Errorf("closure has %d objects, expected manifest/config/layer", count)
	}
	for _, path := range []string{"/api/v1/images?category=invalid", "/api/v1/images?limit=1001", "/api/v1/cache/objects?cleanupPending=invalid"} {
		response, err := support.AdminRequest(ctx, environment, http.MethodGet, path, nil, nil)
		if err != nil {
			return err
		}
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			return fmt.Errorf("invalid inventory request returned %d", response.StatusCode)
		}
	}
	return support.WriteResult(environment, map[string]any{"snapshotID": snapshot, "imageID": id, "objects": count, "category": "solidified"})
}

func read(ctx context.Context, environment testkit.CaseContext, path string) (map[string]any, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("inventory %s returned %s", path, response.Status)
	}
	var result map[string]any
	_, err = support.DecodeJSON(response, 4<<20, &result)
	return result, err
}
