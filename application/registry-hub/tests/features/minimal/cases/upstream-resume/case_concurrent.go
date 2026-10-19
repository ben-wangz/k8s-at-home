package upstreamresume

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

type blobReadResult struct {
	body []byte
	err  error
}

func verifyConcurrentReaders(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient) (map[string]any, error) {
	expected, digest := buildResumeLayer("concurrent")
	started := time.Now()
	first := make(chan blobReadResult, 1)
	go func() {
		body, err := support.ReadBlob(ctx, registry, "github.com/"+upstreamRepository, digest)
		first <- blobReadResult{body: body, err: err}
	}()
	if err := waitForFixtureMarker(ctx, environment, "resume-concurrent", "stage=pause"); err != nil {
		return nil, err
	}
	second := make(chan blobReadResult, 1)
	go func() {
		body, err := support.ReadBlob(ctx, registry, "github.com/"+upstreamRepository, digest)
		second <- blobReadResult{body: body, err: err}
	}()
	firstResult, err := waitReadResult(ctx, first)
	if err != nil {
		return nil, err
	}
	secondResult, err := waitReadResult(ctx, second)
	if err != nil {
		return nil, err
	}
	for _, result := range []blobReadResult{firstResult, secondResult} {
		if result.err != nil || support.Digest(result.body) != digest || len(result.body) != len(expected) {
			return nil, fmt.Errorf("concurrent reader did not receive the complete digest-verified blob: err=%v size=%d", result.err, len(result.body))
		}
	}
	lines, err := fixtureLines(ctx, environment, "resume-concurrent", started)
	if err != nil {
		return nil, err
	}
	if len(lines) != 2 || fixtureSentBytes(lines) != int64(len(expected)) || len(fixtureRangeOffsets(lines)) != 0 {
		return nil, fmt.Errorf("concurrent reads duplicated upstream work: events=%d sent=%d ranges=%v", len(lines), fixtureSentBytes(lines), fixtureRangeOffsets(lines))
	}
	return map[string]any{"digest": digest, "size": len(expected), "readerCount": 2, "upstreamEvents": len(lines), "transmittedBytes": fixtureSentBytes(lines)}, nil
}

func waitReadResult(ctx context.Context, result <-chan blobReadResult) (blobReadResult, error) {
	select {
	case value := <-result:
		return value, nil
	case <-ctx.Done():
		return blobReadResult{}, ctx.Err()
	}
}

func waitForFixtureMarker(ctx context.Context, environment testkit.CaseContext, reference, marker string) error {
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		lines, err := fixtureLines(ctx, environment, reference, time.Now().Add(-3*time.Minute))
		if err == nil {
			for _, line := range lines {
				if strings.Contains(line, marker) {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("fixture did not report %s for %s", marker, reference)
		case <-ticker.C:
		}
	}
}
