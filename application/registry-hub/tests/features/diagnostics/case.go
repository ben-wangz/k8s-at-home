package diagnostics

import (
	"context"
	"embed"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
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
	Domain      string `json:"domain"`
	Name        string `json:"name"`
	ObjectBytes int64  `json:"objectBytes"`
	Objects     int64  `json:"objects"`
}

type domain struct {
	State       string           `json:"state"`
	ObjectBytes *int64           `json:"objectBytes"`
	Objects     *int64           `json:"objects"`
	Categories  map[string]usage `json:"categories"`
}

type report struct {
	SnapshotID  string            `json:"snapshotID"`
	StartedAt   time.Time         `json:"startedAt"`
	CompletedAt time.Time         `json:"completedAt"`
	Domains     map[string]domain `json:"domains"`
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	started := time.Now()
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
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "restart", "deployment/"+environment.Fullname); err != nil {
		return err
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "status", "deployment/"+environment.Fullname, "--timeout=5m"); err != nil {
		return err
	}
	var retained report
	status, err := read(ctx, environment, "/api/v1/storage/summary?snapshotID="+url.QueryEscape(snapshot.SnapshotID), &retained)
	if err != nil || status != http.StatusOK || !reflect.DeepEqual(snapshot, retained) {
		return fmt.Errorf("retained accounting changed across restart: status=%d error=%v", status, err)
	}
	return support.WriteResult(environment, map[string]any{"snapshotID": snapshot.SnapshotID, "domains": snapshot.Domains, "objects": objects, "usageGroups": groups, "elapsedSeconds": time.Since(started).Seconds(), "restart": "immutable_report_preserved"})
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
