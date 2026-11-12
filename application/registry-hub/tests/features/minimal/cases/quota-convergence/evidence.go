package quotaconvergence

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func cacheBlob(ctx context.Context, registry *support.RegistryClient, repository, digest string, body []byte) error {
	response, err := registry.Request(ctx, http.MethodGet, registry.Endpoint(repository, "/blobs/")+digest, nil, nil)
	if err != nil {
		return err
	}
	return support.VerifyResponseBody(response, body, 1<<20)
}

func countUnrelated(ctx context.Context, environment testkit.CaseContext) (int, error) {
	query := url.Values{"authority": {"github.com"}, "repository": {"registry-hub-test/fixture"}, "limit": {"100"}}
	count := 0
	for pages := 0; pages < 100; pages++ {
		response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks?"+query.Encode(), nil, nil)
		if err != nil {
			return count, err
		}
		var page map[string]any
		if _, err := support.DecodeJSON(response, 4<<20, &page); err != nil {
			return count, err
		}
		for _, raw := range testkit.Slice(page["items"]) {
			if strings.HasPrefix(testkit.String(testkit.Map(raw)["reference"]), "quota-unrelated-"+environment.RunID+"-") {
				count++
			}
		}
		token := testkit.String(page["nextToken"])
		if token == "" {
			return count, nil
		}
		query.Set("token", token)
	}
	return count, fmt.Errorf("task pagination exceeded bound")
}

func convergenceTiming(blob, root map[string]any) (map[string]any, error) {
	evidence := map[string]any{}
	times := map[string]time.Time{}
	for name, raw := range map[string]any{"association": blob["protectionAssociatedAt"], "wake": blob["executableAt"], "execution": blob["attemptStartedAt"], "anchor": root["lastProgressAt"]} {
		parsed, err := time.Parse(time.RFC3339Nano, testkit.String(raw))
		if err != nil || parsed.IsZero() {
			return nil, fmt.Errorf("missing %s time: %v", name, raw)
		}
		times[name], evidence[name] = parsed, parsed
	}
	elapsed := times["anchor"].Sub(times["association"])
	if elapsed < 0 || elapsed > 120*time.Second || times["wake"].Before(times["association"]) || times["execution"].Before(times["wake"]) {
		return nil, fmt.Errorf("invalid convergence timeline: %v", evidence)
	}
	evidence["associationToAnchorSeconds"] = elapsed.Seconds()
	return evidence, nil
}

func cancelBlocker(ctx context.Context, environment testkit.CaseContext, id string) error {
	response, err := support.AdminRequest(ctx, environment, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(id)+"/cancel", nil, nil)
	if err != nil {
		return err
	}
	status := response.StatusCode
	response.Body.Close()
	if status != http.StatusAccepted && status != http.StatusOK {
		return fmt.Errorf("cancel quota blocker returned %d", status)
	}
	return nil
}
