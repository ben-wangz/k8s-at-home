package upstreamresume

import (
	"context"
	"fmt"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	upstreamauthtransport "github.com/ben-wangz/registry-hub-tests/features/upstream-auth-transport"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func verifyBackendRestart(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient) (map[string]any, error) {
	started := time.Now()
	requestCtx, cancelRequest := context.WithTimeout(ctx, 5*time.Minute)
	defer cancelRequest()
	result := make(chan blobReadResult, 1)
	expected, digest := buildResumeLayer("restart")
	go func() {
		body, err := support.ReadBlob(requestCtx, registry, "github.com/"+upstreamRepository, digest)
		result <- blobReadResult{body: body, err: err}
	}()
	if err := waitForFixtureMarker(ctx, environment, "resume-restart", "blocked=true"); err != nil {
		cancelRequest()
		<-result
		return nil, err
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "restart", "deployment/"+environment.Fullname); err != nil {
		cancelRequest()
		<-result
		return nil, err
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "status", "deployment/"+environment.Fullname, "--timeout=5m"); err != nil {
		cancelRequest()
		<-result
		return nil, err
	}
	if err := upstreamauthtransport.WaitBackendServiceEndpoint(ctx, environment); err != nil {
		cancelRequest()
		<-result
		return nil, err
	}
	firstAttempt, completed := receiveBlobRead(ctx, result, 15*time.Second)
	if !completed {
		cancelRequest()
		firstAttempt = <-result
	}
	body, err := support.ReadBlob(ctx, registry, "github.com/"+upstreamRepository, digest)
	if err != nil || len(body) != len(expected) || support.Digest(body) != digest {
		return nil, fmt.Errorf("Blob did not complete after Backend restart: err=%v size=%d", err, len(body))
	}
	lines, err := fixtureLines(ctx, environment, "resume-restart", started)
	if err != nil {
		return nil, err
	}
	offsets := fixtureRangeOffsets(lines)
	if !equalOffsets(offsets, []int64{1 << 20, 1 << 20}) {
		return nil, fmt.Errorf("Backend restart did not reuse the committed offset: Range starts=%v", offsets)
	}
	return map[string]any{
		"digest": digest, "size": len(body), "rangeOffsets": offsets,
		"firstRequestInterrupted": firstAttempt.err != nil, "firstRequestError": errorText(firstAttempt.err),
		"backendRestarted": true,
	}, nil
}

func receiveBlobRead(ctx context.Context, result <-chan blobReadResult, timeout time.Duration) (blobReadResult, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case value := <-result:
		return value, true
	case <-ctx.Done():
		return blobReadResult{err: ctx.Err()}, true
	case <-timer.C:
		return blobReadResult{}, false
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
