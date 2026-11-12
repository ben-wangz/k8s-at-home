package upstreammirror

import (
	"context"
	"embed"
	"errors"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"io/fs"
	"time"
)

//go:embed persistence/case.json
var persistenceMetadata embed.FS

func NewPersistenceFixture() (testkit.Case, error) {
	metadata, err := fs.Sub(persistenceMetadata, "persistence")
	if err != nil {
		return testkit.Case{}, err
	}
	definition, raw, err := testkit.LoadDefinition(metadata, "upstream-persistence-fixture")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: runPersistenceFixture}, nil
}

func runPersistenceFixture(ctx context.Context, environment testkit.CaseContext) (err error) {
	var run *mirrorRun
	if err := testkit.RunPhase(environment, "fixture-setup", func() error {
		var err error
		run, err = setupMirrorRun(ctx, environment)
		return err
	}); err != nil {
		return err
	}
	if err := captureLatencyEvidence(ctx, environment, "before"); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, captureLatencyEvidence(ctx, environment, "after")) }()
	var result map[string]any
	if err := testkit.RunPhase(environment, "healthy-solidification", func() error {
		bounded, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		run.context = bounded
		var err error
		result, err = verifyPersistencePull(run)
		return err
	}); err != nil {
		return err
	}
	return support.WriteResult(environment, result)
}
