package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"
)

func validateGCContainers(actualPod, expectedPod map[string]any) bool {
	actualContainers, actualOK := anySlice(actualPod["containers"])
	expectedContainers, expectedOK := anySlice(expectedPod["containers"])
	if !actualOK || !expectedOK || len(actualContainers) != 1 || len(expectedContainers) != 1 {
		return false
	}
	for _, field := range []string{"initContainers", "ephemeralContainers"} {
		if containers, ok := anySlice(actualPod[field]); ok && len(containers) != 0 {
			return false
		}
	}
	actual, actualOK := stringAnyMap(actualContainers[0])
	expected, expectedOK := stringAnyMap(expectedContainers[0])
	if !actualOK || !expectedOK {
		return false
	}
	for _, field := range []string{"name", "image", "imagePullPolicy", "command", "args", "volumeMounts", "envFrom", "securityContext"} {
		if !jsonValuesEqual(actual[field], expected[field]) {
			return false
		}
	}
	for _, field := range []string{"env", "lifecycle", "livenessProbe", "readinessProbe", "startupProbe", "ports", "volumeDevices"} {
		if value, found := actual[field]; found && !emptyJSONValue(value) {
			return false
		}
	}
	for field := range actual {
		switch field {
		case "name", "image", "imagePullPolicy", "command", "args", "volumeMounts", "envFrom", "securityContext", "resources", "terminationMessagePath", "terminationMessagePolicy":
		default:
			return false
		}
	}
	return true
}

func validateGCVolumes(actualValue, expectedValue any) bool {
	actualVolumes, actualOK := anySlice(actualValue)
	expectedVolumes, expectedOK := anySlice(expectedValue)
	if !actualOK || !expectedOK || len(actualVolumes) != len(expectedVolumes) {
		return false
	}
	actualByName := make(map[string]map[string]any, len(actualVolumes))
	for _, value := range actualVolumes {
		volume, ok := stringAnyMap(value)
		if !ok {
			return false
		}
		name, _ := volume["name"].(string)
		if name == "" || actualByName[name] != nil {
			return false
		}
		actualByName[name] = volume
	}
	for _, value := range expectedVolumes {
		expected, ok := stringAnyMap(value)
		if !ok {
			return false
		}
		name, _ := expected["name"].(string)
		actual := actualByName[name]
		if actual == nil || len(actual) != len(expected) {
			return false
		}
		source := ""
		for field := range expected {
			if field != "name" {
				source = field
				break
			}
		}
		if source == "" || !validateGCVolumeSource(source, actual[source], expected[source]) {
			return false
		}
	}
	return true
}

func validateGCVolumeSource(source string, actualValue, expectedValue any) bool {
	actual, actualOK := stringAnyMap(actualValue)
	expected, expectedOK := stringAnyMap(expectedValue)
	if !actualOK || !expectedOK {
		return false
	}
	switch source {
	case "configMap":
		for _, field := range []string{"name", "items", "defaultMode"} {
			if !jsonValuesEqual(actual[field], expected[field]) {
				return false
			}
		}
		if optional, _ := actual["optional"].(bool); optional {
			return false
		}
		for field := range actual {
			if field != "name" && field != "items" && field != "defaultMode" && field != "optional" {
				return false
			}
		}
		return true
	case "persistentVolumeClaim":
		if !jsonValuesEqual(actual["claimName"], expected["claimName"]) {
			return false
		}
		if readOnly, _ := actual["readOnly"].(bool); readOnly {
			return false
		}
		for field := range actual {
			if field != "claimName" && field != "readOnly" {
				return false
			}
		}
		return true
	case "emptyDir":
		for field, value := range actual {
			if field == "medium" {
				if value != "" {
					return false
				}
				continue
			}
			if field != "sizeLimit" || value != nil {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func jsonValuesEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func stringAnyMap(value any) (map[string]any, bool) {
	result, ok := value.(map[string]any)
	return result, ok
}

func anySlice(value any) ([]any, bool) {
	switch result := value.(type) {
	case []any:
		return result, true
	case []map[string]any:
		items := make([]any, len(result))
		for index := range result {
			items[index] = result[index]
		}
		return items, true
	default:
		return nil, false
	}
}

func emptyJSONValue(value any) bool {
	if value == nil {
		return true
	}
	if items, ok := anySlice(value); ok {
		return len(items) == 0
	}
	return false
}

func (c *gcController) recordGCJobUID(ctx context.Context, runID, jobUID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.run.ID != runID || c.run.JobUID != "" && c.run.JobUID != jobUID {
		return errors.New("GC execution identity changed while recording Job UID")
	}
	if c.run.JobUID == jobUID {
		return nil
	}
	previousTime := c.run.UpdatedAt
	c.run.JobUID = jobUID
	c.run.UpdatedAt = time.Now().UTC()
	if err := c.persistLocked(ctx); err != nil {
		c.run.JobUID = ""
		c.run.UpdatedAt = previousTime
		return err
	}
	return nil
}
