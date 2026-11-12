package testkit

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPhaseEvidenceSurvivesFailure(t *testing.T) {
	environment := CaseContext{OutputDir: t.TempDir()}
	want := errors.New("fixture failed")
	if err := RunPhase(environment, "fixture", func() error { return want }); !errors.Is(err, want) {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(environment.OutputDir, "phases.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("phase events=%d", len(lines))
	}
	for i, status := range []string{"running", "failed"} {
		var event map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &event); err != nil {
			t.Fatal(err)
		}
		if event["status"] != status || event["name"] != "fixture" || event["observedAt"] == nil {
			t.Fatalf("missing evidence: %v", event)
		}
	}
}
