package testkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func RunPhase(environment CaseContext, name string, run func() error) error {
	started := time.Now()
	if err := recordPhase(environment, name, "running", started, 0); err != nil {
		return err
	}
	fmt.Printf("phase %s started\n", name)
	err := run()
	status := "passed"
	if err != nil {
		status = "failed"
	}
	elapsed := time.Since(started)
	fmt.Printf("phase %s %s in %s\n", name, status, elapsed.Round(time.Millisecond))
	return errors.Join(err, recordPhase(environment, name, status, started, elapsed))
}

func recordPhase(environment CaseContext, name, status string, started time.Time, elapsed time.Duration) error {
	if err := os.MkdirAll(environment.OutputDir, 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(environment.OutputDir, "phases.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(file).Encode(struct {
		Name            string    `json:"name"`
		Status          string    `json:"status"`
		StartedAt       time.Time `json:"startedAt"`
		ObservedAt      time.Time `json:"observedAt"`
		DurationSeconds float64   `json:"durationSeconds"`
	}{name, status, started.UTC(), time.Now().UTC(), elapsed.Seconds()})
	return errors.Join(err, file.Close())
}
