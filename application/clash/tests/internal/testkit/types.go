package testkit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type Check struct {
	ID               string                `json:"id"`
	Description      string                `json:"description"`
	ExpectedEvidence []EvidenceExpectation `json:"expectedEvidence"`
}

type EvidenceExpectation struct {
	ID    string   `json:"id"`
	Modes []string `json:"modes,omitempty"`
}

type Definition struct {
	ID             string   `json:"id"`
	Description    string   `json:"description"`
	DependsOn      []string `json:"dependsOn"`
	TimeoutSeconds int      `json:"timeoutSeconds"`
	Mode           string   `json:"mode"`
	Spec           string   `json:"spec,omitempty"`
	Preconditions  []string `json:"preconditions"`
	Requirements   []string `json:"requirements"`
	Checks         []Check  `json:"checks"`
}

type Result struct {
	ID       string     `json:"id"`
	Status   string     `json:"status"`
	Evidence []Evidence `json:"evidence,omitempty"`
	Error    string     `json:"error,omitempty"`
}

type Evidence struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type CaseResult struct {
	ID       string    `json:"id"`
	Status   string    `json:"status"`
	Checks   []Result  `json:"checks"`
	Error    string    `json:"error,omitempty"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
}

func ReadJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func WriteJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	_, writeErr := file.Write(append(data, '\n'))
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}

func Command(ctx context.Context, directory string, environment []string, program string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, program, args...)
	command.Dir = directory
	command.Env = append(os.Environ(), environment...)
	output, err := command.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%s failed: %w", program, err)
	}
	return output, nil
}
