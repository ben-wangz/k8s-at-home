package diagnostics

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

type object struct {
	ID          string `json:"id"`
	Domain      string `json:"domain"`
	Category    string `json:"category"`
	Digest      string `json:"digest"`
	ObjectBytes int64  `json:"objectBytes"`
}

func verifyObjects(ctx context.Context, environment testkit.CaseContext, snapshot report) (int, error) {
	query := url.Values{"snapshotID": {snapshot.SnapshotID}, "limit": {"32"}}
	seen := map[string]bool{}
	fixture := map[string]bool{}
	for _, field := range []string{"manifestDigest", "configDigest", "layerDigest"} {
		digest, err := support.DetailString(environment.DependencyResults["registry-roundtrip"], field)
		if err != nil {
			return 0, err
		}
		fixture[digest] = false
	}
	totals := map[string]usage{}
	for {
		var page struct {
			SnapshotID string   `json:"snapshotID"`
			Items      []object `json:"items"`
			NextToken  string   `json:"nextToken"`
		}
		status, err := read(ctx, environment, "/api/v1/storage/objects?"+query.Encode(), &page)
		if err != nil || status != http.StatusOK || page.SnapshotID != snapshot.SnapshotID || len(page.Items) > 32 {
			return 0, fmt.Errorf("invalid object page: status=%d error=%v", status, err)
		}
		for _, item := range page.Items {
			if item.ID == "" || seen[item.ID] || item.ObjectBytes < 0 {
				return 0, fmt.Errorf("invalid or duplicate physical object %q", item.ID)
			}
			seen[item.ID] = true
			if _, expected := fixture[item.Digest]; expected && item.Domain == "registry" && item.Category == "registry_payloads" {
				fixture[item.Digest] = true
			}
			key := item.Domain + "/" + item.Category
			total := totals[key]
			total.ObjectBytes, total.Objects = total.ObjectBytes+item.ObjectBytes, total.Objects+1
			totals[key] = total
		}
		if page.NextToken == "" {
			break
		}
		query.Set("token", page.NextToken)
	}
	for name, value := range snapshot.Domains {
		for category, expected := range value.Categories {
			actual := totals[name+"/"+category]
			if actual.ObjectBytes != expected.ObjectBytes || actual.Objects != expected.Objects {
				return 0, fmt.Errorf("object accounting differs for %s/%s", name, category)
			}
			delete(totals, name+"/"+category)
		}
	}
	if len(totals) != 0 {
		return 0, fmt.Errorf("physical objects have categories missing from summary")
	}
	for digest, found := range fixture {
		if !found {
			return 0, fmt.Errorf("uploaded fixture payload %s is missing from physical accounting", digest)
		}
	}
	return len(seen), nil
}

func verifyUsage(ctx context.Context, environment testkit.CaseContext, snapshot report) (int, error) {
	query := url.Values{"snapshotID": {snapshot.SnapshotID}, "dimension": {"category"}, "limit": {"32"}}
	seen := map[string]bool{}
	previous := int64(^uint64(0) >> 1)
	for {
		var page struct {
			SnapshotID string  `json:"snapshotID"`
			Items      []usage `json:"items"`
			NextToken  string  `json:"nextToken"`
		}
		status, err := read(ctx, environment, "/api/v1/storage/usage?"+query.Encode(), &page)
		if err != nil || status != http.StatusOK || page.SnapshotID != snapshot.SnapshotID || len(page.Items) > 32 {
			return 0, fmt.Errorf("invalid usage page: status=%d error=%v", status, err)
		}
		for _, item := range page.Items {
			key := item.Domain + "/" + item.Name
			expected, ok := snapshot.Domains[item.Domain].Categories[item.Name]
			if !ok || seen[key] || item.ObjectBytes > previous || item.ObjectBytes != expected.ObjectBytes || item.Objects != expected.Objects {
				return 0, fmt.Errorf("usage totals, identity or descending order differ for %s", key)
			}
			seen[key], previous = true, item.ObjectBytes
		}
		if page.NextToken == "" {
			break
		}
		query.Set("token", page.NextToken)
	}
	expected := 0
	for _, value := range snapshot.Domains {
		expected += len(value.Categories)
	}
	if len(seen) != expected {
		return 0, fmt.Errorf("usage enumerated %d categories, expected %d", len(seen), expected)
	}
	return len(seen), nil
}
