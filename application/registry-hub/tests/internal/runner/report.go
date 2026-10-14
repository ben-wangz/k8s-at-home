package runner

import (
	"fmt"
	"path/filepath"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func saveRecord(stateDir string, record testkit.RunRecord) error {
	if err := testkit.WriteJSONAtomic(filepath.Join(stateDir, "run.json"), record); err != nil {
		return fmt.Errorf("save run report: %w", err)
	}
	return nil
}

func readRecord(stateDir string) (testkit.RunRecord, error) {
	var record testkit.RunRecord
	if err := testkit.ReadJSON(filepath.Join(stateDir, "run.json"), &record); err != nil {
		return testkit.RunRecord{}, err
	}
	return record, nil
}
