package environment

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/ben-wangz/clash-tests/internal/testkit"
)

func execute(ctx context.Context, directory string, input []byte, variables []string, program string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, program, args...)
	command.Dir = directory
	command.Env = append(os.Environ(), variables...)
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%s command failed: %w", program, err)
	}
	return output, nil
}

func Save(directory string, record *Record) error {
	return testkit.WriteJSON(filepath.Join(directory, "run.json"), record)
}

func Load(id string) (string, *Record, error) {
	if !ValidID(id) {
		return "", nil, fmt.Errorf("invalid run ID")
	}
	paths, err := filepath.Glob(filepath.Join(StateRoot(), "*", id, "run.json"))
	if err != nil {
		return "", nil, err
	}
	if len(paths) != 1 {
		return "", nil, fmt.Errorf("expected one saved run for %s; found %d", id, len(paths))
	}
	var record Record
	if err := testkit.ReadJSON(paths[0], &record); err != nil {
		return "", nil, err
	}
	if record.ID != id {
		return "", nil, fmt.Errorf("run record identity mismatch")
	}
	return filepath.Dir(paths[0]), &record, nil
}
