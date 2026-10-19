package upstreamresume

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func verifyResumeBlob(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, mode string, expectedOffsets []int64) (map[string]any, error) {
	expected, digest := buildResumeLayer(mode)
	started := time.Now()
	blob, err := support.ReadBlob(ctx, registry, "github.com/"+upstreamRepository, digest)
	if err != nil {
		return nil, err
	}
	elapsed := time.Since(started)
	if len(blob) != len(expected) || support.Digest(blob) != digest || !bytes.Equal(blob, expected) {
		return nil, fmt.Errorf("completed blob failed size or digest verification: size=%d want=%d digest=%s", len(blob), len(expected), support.Digest(blob))
	}
	lines, err := fixtureLines(ctx, environment, "resume-"+mode, started)
	if err != nil {
		return nil, err
	}
	offsets := fixtureRangeOffsets(lines)
	if !equalOffsets(offsets, expectedOffsets) {
		return nil, fmt.Errorf("Range offsets are %v, want %v", offsets, expectedOffsets)
	}
	if mode == "healthy" && fixtureSentBytes(lines) != int64(len(expected)) {
		return nil, fmt.Errorf("healthy transfer sent %d bytes, want %d", fixtureSentBytes(lines), len(expected))
	}
	if mode == "no-range" && countFixtureStatus(lines, "status=200") < 1 {
		return nil, fmt.Errorf("Range fallback did not complete with a full 200 response")
	}
	if mode == "etag-change" && countFixtureStatus(lines, "status=206") != 1 {
		return nil, fmt.Errorf("changed ETag did not trigger one bounded Range response")
	}
	if mode == "range-416" && countFixtureStatus(lines, "status=416") != 1 {
		return nil, fmt.Errorf("unsatisfied Range fixture response was not observed")
	}
	if mode == "idle" {
		logText := strings.Join(lines, "\n")
		if elapsed < 800*time.Millisecond || elapsed > 4500*time.Millisecond || !strings.Contains(logText, "stage=idle") || !strings.Contains(logText, "cancelled=true") {
			return nil, fmt.Errorf("idle timeout evidence is invalid: elapsed=%s", elapsed)
		}
	}
	return map[string]any{
		"reference": "resume-" + mode, "digest": digest, "size": len(blob),
		"rangeOffsets": offsets, "transmittedBytes": fixtureSentBytes(lines),
		"elapsedMillis": elapsed.Milliseconds(), "fixtureRequests": len(lines),
		"status200": countFixtureStatus(lines, "status=200"), "status206": countFixtureStatus(lines, "status=206"),
	}, nil
}

func buildResumeLayer(mode string) ([]byte, string) {
	return support.BuildResumeLayerFixture(mode)
}

func fixtureLines(ctx context.Context, environment testkit.CaseContext, reference string, since time.Time) ([]string, error) {
	proxyPod := "pod/" + environment.Release + "-external-upstream-proxy"
	result, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", proxyPod, "-c", "proxy", "--since-time="+since.UTC().Format(time.RFC3339Nano), "--tail=20000")
	if err != nil {
		return nil, err
	}
	marker := "resume_fixture reference=" + reference + " "
	var lines []string
	for _, line := range strings.Split(string(result.Stdout), "\n") {
		if strings.Contains(line, marker) {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("fixture produced no evidence for %s", reference)
	}
	return lines, nil
}

func fixtureRangeOffsets(lines []string) []int64 {
	var offsets []int64
	for _, line := range lines {
		marker := `range="bytes=`
		start := strings.Index(line, marker)
		if start < 0 {
			continue
		}
		value := line[start+len(marker):]
		end := strings.IndexByte(value, '-')
		if end < 0 {
			continue
		}
		offset, err := strconv.ParseInt(value[:end], 10, 64)
		if err == nil {
			offsets = append(offsets, offset)
		}
	}
	return offsets
}

func fixtureSentBytes(lines []string) int64 {
	var total int64
	for _, line := range lines {
		for _, field := range strings.Fields(line) {
			if !strings.HasPrefix(field, "sent=") {
				continue
			}
			sent, err := strconv.ParseInt(strings.TrimPrefix(field, "sent="), 10, 64)
			if err == nil {
				total += sent
			}
		}
	}
	return total
}

func countFixtureStatus(lines []string, status string) int {
	count := 0
	for _, line := range lines {
		if strings.Contains(line, status) {
			count++
		}
	}
	return count
}

func equalOffsets(left, right []int64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
