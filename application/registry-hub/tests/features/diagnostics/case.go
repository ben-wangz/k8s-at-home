package diagnostics

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "feature-diagnostics")
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, err
}

type usage struct {
	Domain         string `json:"domain"`
	Name           string `json:"name"`
	ObjectBytes    int64  `json:"objectBytes"`
	Objects        int64  `json:"objects"`
	LogicalBytes   *int64 `json:"logicalBytes"`
	LogicalUnknown bool   `json:"logicalUnknown"`
}

type domain struct {
	State                 string           `json:"state"`
	ObjectBytes           *int64           `json:"objectBytes"`
	Objects               *int64           `json:"objects"`
	Categories            map[string]usage `json:"categories"`
	SummaryOnlyCategories []string         `json:"summaryOnlyCategories"`
}

type report struct {
	SnapshotID  string            `json:"snapshotID"`
	StartedAt   time.Time         `json:"startedAt"`
	CompletedAt time.Time         `json:"completedAt"`
	Domains     map[string]domain `json:"domains"`
	Cost        struct {
		InputObjects       int64 `json:"inputObjects"`
		SummaryOnlyObjects int64 `json:"summaryOnlyObjects"`
		OutputObjects      int64 `json:"outputObjects"`
		OutputBytes        int64 `json:"outputBytes"`
	} `json:"cost"`
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	started := time.Now()
	var collection map[string]any
	status, err := read(ctx, environment, "/api/v1/storage/status", &collection)
	if err != nil || status != http.StatusOK || collection["phase"] == nil || collection["totalObjects"] != nil || collection["percentComplete"] != nil {
		return fmt.Errorf("independent collector status invalid: status=%d error=%v", status, err)
	}
	dependencyTasks, err := verifyDependencyTaskObservations(ctx, environment)
	if err != nil {
		return err
	}
	snapshot, err := awaitReport(ctx, environment)
	if err != nil {
		return err
	}
	objects, err := verifyObjects(ctx, environment, snapshot)
	if err != nil {
		return err
	}
	groups, err := verifyUsage(ctx, environment, snapshot)
	if err != nil {
		return err
	}
	logicalBytes, err := verifyFixtureLogicalBytes(ctx, environment, snapshot)
	if err != nil {
		return err
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "restart", "deployment/"+environment.Fullname); err != nil {
		return err
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "status", "deployment/"+environment.Fullname, "--timeout=5m"); err != nil {
		return err
	}
	var retained report
	status, err = read(ctx, environment, "/api/v1/storage/summary?snapshotID="+url.QueryEscape(snapshot.SnapshotID), &retained)
	if err != nil || status != http.StatusOK || !reflect.DeepEqual(snapshot, retained) {
		return fmt.Errorf("retained accounting changed across restart: status=%d error=%v", status, err)
	}
	details := map[string]any{"snapshotID": snapshot.SnapshotID, "domains": snapshot.Domains, "cost": snapshot.Cost, "objects": objects, "usageGroups": groups, "fixtureLogicalBytes": logicalBytes, "dependencyTaskObservations": dependencyTasks, "elapsedSeconds": time.Since(started).Seconds(), "restart": "immutable_report_preserved"}
	if environment.ScenarioID == "feature-diagnostics-s3" {
		seededRecovery, err := verifySeededStorageRecoveryS3(ctx, environment, snapshot)
		if err != nil {
			return err
		}
		details["seededS3Recovery"] = seededRecovery
	}
	return support.WriteResult(environment, details)
}

func verifyFixtureLogicalBytes(ctx context.Context, environment testkit.CaseContext, snapshot report) (int64, error) {
	repository, err := support.DetailString(environment.DependencyResults["registry-roundtrip"], "repository")
	if err != nil {
		return 0, err
	}
	client := support.NewRegistryClient(environment)
	manifestBytes, _, err := support.ReadManifest(ctx, client, repository, "minimal", support.OCIManifestMediaType)
	if err != nil {
		return 0, err
	}
	var manifest support.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil || manifest.Config == nil {
		return 0, fmt.Errorf("diagnostics fixture manifest is invalid: %v", err)
	}
	expected := int64(len(manifestBytes) + manifest.Config.Size)
	for _, layer := range manifest.Layers {
		expected += int64(layer.Size)
	}
	query := url.Values{"snapshotID": {snapshot.SnapshotID}, "domain": {"registry"}, "dimension": {"repository"}, "repository": {repository}}
	query.Set("limit", "32")
	var matching []usage
	for {
		var page struct {
			SnapshotID string  `json:"snapshotID"`
			Items      []usage `json:"items"`
			NextToken  string  `json:"nextToken"`
		}
		status, err := read(ctx, environment, "/api/v1/storage/usage?"+query.Encode(), &page)
		if err != nil || status != http.StatusOK || page.SnapshotID != snapshot.SnapshotID {
			return 0, fmt.Errorf("fixture logical usage query failed: status=%d error=%v", status, err)
		}
		matching = append(matching, page.Items...)
		if page.NextToken == "" {
			break
		}
		query.Set("token", page.NextToken)
	}
	if len(matching) != 1 {
		return 0, fmt.Errorf("fixture logical usage query returned %d rows, want exactly one", len(matching))
	}
	actual := matching[0]
	minimumObjects := int64(2 + len(manifest.Layers))
	if actual.Objects < minimumObjects || actual.LogicalUnknown || actual.LogicalBytes == nil || *actual.LogicalBytes != expected {
		return 0, fmt.Errorf("fixture logical usage = physical objects:%d bytes:%s unknown:%t; want at least %d objects and logical bytes:%d", actual.Objects, nullableInt64(actual.LogicalBytes), actual.LogicalUnknown, minimumObjects, expected)
	}
	return expected, nil
}

func nullableInt64(value *int64) string {
	if value == nil {
		return "unknown"
	}
	return strconv.FormatInt(*value, 10)
}

func read(ctx context.Context, environment testkit.CaseContext, path string, result any) (int, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, path, nil, nil)
	if err != nil {
		return 0, err
	}
	status := response.StatusCode
	_, err = support.DecodeJSON(response, 4<<20, result)
	return status, err
}

func awaitReport(ctx context.Context, environment testkit.CaseContext) (report, error) {
	var pending error
	for {
		var snapshot report
		status, err := read(ctx, environment, "/api/v1/storage/summary", &snapshot)
		if err != nil {
			return report{}, err
		}
		if status == http.StatusOK {
			if err := verifySummaryCoverage(snapshot); err != nil {
				return report{}, err
			}
			if snapshot.SnapshotID == "" || snapshot.StartedAt.IsZero() || snapshot.CompletedAt.IsZero() || len(snapshot.Domains) != 2 {
				return report{}, fmt.Errorf("storage report lacks publication evidence")
			}
			ready := true
			for name, value := range snapshot.Domains {
				if value.State != "observed" || value.ObjectBytes == nil || value.Objects == nil {
					return report{}, fmt.Errorf("storage domain %s is not observed", name)
				}
				ready = ready && *value.Objects > 0
				var bytes, objects int64
				for _, category := range value.Categories {
					bytes, objects = bytes+category.ObjectBytes, objects+category.Objects
				}
				if bytes != *value.ObjectBytes || objects != *value.Objects {
					return report{}, fmt.Errorf("category totals do not reconcile for %s", name)
				}
			}
			if ready {
				pending = verifyRegistryFiles(ctx, environment, snapshot)
				if pending == nil {
					return snapshot, nil
				}
			} else {
				pending = fmt.Errorf("report predates the uploaded fixture")
			}
			if snapshot.StartedAt.After(environment.DependencyResults["registry-roundtrip"].Finished) {
				return report{}, fmt.Errorf("post-upload collection is inconsistent: %w", pending)
			}
		}
		if status != http.StatusServiceUnavailable && status != http.StatusOK {
			return report{}, fmt.Errorf("storage summary returned %d", status)
		}
		select {
		case <-ctx.Done():
			return report{}, fmt.Errorf("completed accounting report for the uploaded fixture was not published: %w; last observation: %v", ctx.Err(), pending)
		case <-time.After(2 * time.Second):
		}
	}
}
