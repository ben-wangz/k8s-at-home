package diagnostics

import (
	"context"
	"fmt"
	"net/url"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func verifyDependencyTaskObservations(ctx context.Context, environment testkit.CaseContext) (map[string]string, error) {
	fixture := environment.DependencyResults["upstream-mirror"]
	rootDigest, err := support.DetailString(fixture, "missingURLDiscoveredRootDigest")
	if err != nil {
		return nil, err
	}
	rootTaskID, err := support.DetailString(fixture, "missingURLDiscoveredTaskID")
	if err != nil {
		return nil, err
	}
	blobDigest, err := support.DetailString(fixture, "missingURLBlobDigest")
	if err != nil {
		return nil, err
	}
	if rootTaskID == "" || blobDigest == "" {
		return nil, fmt.Errorf("upstream fixture did not record its missing-URL root and blob identity")
	}
	var root map[string]any
	status, err := read(ctx, environment, "/api/v1/tasks/"+url.PathEscape(rootTaskID), &root)
	if err != nil || status != 200 {
		return nil, fmt.Errorf("read missing-URL root task %s: status=%d: %v", rootTaskID, status, err)
	}
	if testkit.String(root["id"]) != rootTaskID || testkit.String(root["rootDigest"]) != rootDigest || testkit.String(root["reference"]) != "minimal-url-missing" || root["rootPublished"] == true {
		return nil, fmt.Errorf("missing-URL root task %s identity or publication changed: digest=%q status=%q rootPublished=%v reference=%q", rootTaskID,
			testkit.String(root["rootDigest"]), testkit.String(root["status"]), root["rootPublished"], testkit.String(root["reference"]))
	}

	var dependencies struct {
		Items []map[string]any `json:"items"`
	}
	status, err = read(ctx, environment, "/api/v1/tasks/"+url.PathEscape(rootTaskID)+"/dependencies?limit=32", &dependencies)
	if err != nil || status != 200 {
		return nil, fmt.Errorf("read missing-URL root dependencies: status=%d: %v", status, err)
	}
	for _, dependency := range dependencies.Items {
		if testkit.String(dependency["digest"]) != blobDigest || testkit.String(dependency["role"]) != "blob" {
			continue
		}
		observation := testkit.String(dependency["taskObservation"])
		intentID, taskID := testkit.String(dependency["intentID"]), testkit.String(dependency["taskID"])
		taskURL, intentURL := testkit.String(dependency["taskURL"]), testkit.String(dependency["intentURL"])
		result := map[string]string{
			"rootTaskID": rootTaskID, "rootStatus": testkit.String(root["status"]), "rootDigest": rootDigest,
			"intentID": intentID, "taskID": taskID, "blobDigest": blobDigest,
			"taskObservation": observation,
		}
		if observation == "" && intentID == "" && taskID == "" && taskURL == "" && intentURL == "" {
			result["taskObservation"] = "unlinked"
			return result, nil
		}
		if intentID == "" || intentURL != "/api/v1/blob-intents/"+url.PathEscape(intentID) || taskURL != "/api/v1/tasks/"+url.PathEscape(intentID) {
			return nil, fmt.Errorf("dependency intent links are incomplete: root=%s intent=%s task=%s observation=%s", rootTaskID, intentID, taskID, observation)
		}
		var intent map[string]any
		status, err = read(ctx, environment, intentURL, &intent)
		if err != nil || status != 200 {
			return nil, fmt.Errorf("read linked blob intent %s: status=%d: %v", intentID, status, err)
		}
		identity := testkit.Map(intent["identity"])
		if testkit.String(intent["id"]) != intentID || testkit.String(identity["authority"]) != testkit.String(root["authority"]) ||
			testkit.String(identity["repository"]) != testkit.String(root["repository"]) || testkit.String(identity["digest"]) != blobDigest {
			return nil, fmt.Errorf("dependency intent detail identity differs from the observed blob edge: intent=%s digest=%s", intentID, blobDigest)
		}
		result["observedIntentState"], result["currentIntentState"] = testkit.String(dependency["intentState"]), testkit.String(intent["state"])
		switch observation {
		case "found":
			if taskID != intentID {
				return nil, fmt.Errorf("found task ID %s differs from intent ID %s", taskID, intentID)
			}
			var task map[string]any
			status, err = read(ctx, environment, taskURL, &task)
			if err != nil || status != 200 {
				return nil, fmt.Errorf("read linked blob task %s: status=%d: %v", taskID, status, err)
			}
			if testkit.String(task["id"]) != taskID || testkit.String(task["blobDigest"]) != blobDigest {
				return nil, fmt.Errorf("dependency task detail identity differs from the observed blob edge: task=%s digest=%s", taskID, blobDigest)
			}
			result["observedTaskStatus"], result["currentTaskStatus"] = testkit.String(dependency["taskStatus"]), testkit.String(task["status"])
		case "absent":
			if taskID != "" {
				return nil, fmt.Errorf("absent task observation unexpectedly includes task ID %s", taskID)
			}
			var missingTask map[string]any
			status, err = read(ctx, environment, taskURL, &missingTask)
			if err != nil || status != 404 {
				return nil, fmt.Errorf("absent task detail returned status=%d: %v", status, err)
			}
			result["currentTaskStatus"] = "absent"
		default:
			return nil, fmt.Errorf("requested missing-URL blob %s has unsupported task observation %q", blobDigest, observation)
		}
		return result, nil
	}
	return nil, fmt.Errorf("missing-URL root task %s has no dependency edge for requested blob %s", rootTaskID, blobDigest)
}
