package startupwait

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"os"
)

func withRecordState(content, state string) (string, error) {
	var record map[string]any
	if err := json.Unmarshal([]byte(content), &record); err != nil {
		return "", fmt.Errorf("initialization record is invalid")
	}
	if testkit.String(record["state"]) != "ready" {
		return "", fmt.Errorf("refusing to modify an initialization record that is not ready")
	}
	record["state"] = state
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func patchRecord(ctx context.Context, namespace, name, content string) error {
	patch, err := json.Marshal(map[string]any{"data": map[string]string{"record.json": content}})
	if err != nil {
		return err
	}
	_, err = testkit.Command(ctx, "kubectl", "-n", namespace, "patch", "configmap", name, "--type=merge", "-p", string(patch))
	return err
}

func snapshotRecord(object testkit.Object, managedBy string) recordSnapshot {
	return recordSnapshot{
		Name: object.Name, UID: object.UID, ManagedBy: managedBy,
		Content: testkit.String(object.Data["record.json"]),
	}
}

func recoverInterruptedRun(ctx context.Context, environment testkit.CaseContext, journalPath string, records ...testkit.Object) error {
	var journal recoveryJournal
	err := testkit.ReadJSON(journalPath, &journal)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(journal.Records) != 2 || len(records) != 2 {
		return fmt.Errorf("startup-wait recovery journal has an invalid record count")
	}
	for _, expected := range records {
		found := false
		for _, snapshot := range journal.Records {
			if snapshot.Name == expected.Name && snapshot.UID == expected.UID &&
				snapshot.ManagedBy == expected.Labels["registry-hub.io/managed-by"] && snapshot.Content != "" {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("startup-wait recovery journal does not match current initialization record identities")
		}
	}
	return restoreJournal(ctx, environment, journalPath, journal)
}

func restoreJournal(ctx context.Context, environment testkit.CaseContext, journalPath string, journal recoveryJournal) error {
	for _, snapshot := range journal.Records {
		object, err := testkit.KubectlGet(ctx, environment.Namespace, "configmap", snapshot.Name)
		if err != nil {
			return err
		}
		metadata := testkit.Map(object["metadata"])
		labels := testkit.Map(metadata["labels"])
		if testkit.String(metadata["uid"]) != snapshot.UID ||
			testkit.String(labels["registry-hub.io/instance"]) != environment.Fullname ||
			testkit.String(labels["registry-hub.io/managed-by"]) != snapshot.ManagedBy {
			return fmt.Errorf("initialization record %s identity changed; refusing recovery write", snapshot.Name)
		}
		if err := validateReadyRecord(snapshot.Content, environment.Fullname, snapshot.ManagedBy); err != nil {
			return err
		}
		if err := patchRecord(ctx, environment.Namespace, snapshot.Name, snapshot.Content); err != nil {
			return err
		}
	}
	for _, deployment := range []string{environment.Fullname, environment.Fullname + "-registry"} {
		if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "status", "deployment/"+deployment, "--timeout=180s"); err != nil {
			return fmt.Errorf("deployment %s did not recover from initialization wait: %w", deployment, err)
		}
	}
	if err := os.Remove(journalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func validateReadyRecord(content, fullname, managedBy string) error {
	var record map[string]any
	if err := json.Unmarshal([]byte(content), &record); err != nil {
		return fmt.Errorf("startup-wait recovery record is invalid")
	}
	expectedProtocol := "v1"
	if managedBy == "admin-initializer" {
		expectedProtocol = "admin-v1"
	}
	if testkit.String(record["protocol"]) != expectedProtocol ||
		testkit.String(record["instance"]) != fullname ||
		testkit.String(record["state"]) != "ready" ||
		testkit.String(record["secretUID"]) == "" {
		return fmt.Errorf("startup-wait recovery record does not contain a valid ready state")
	}
	return nil
}
