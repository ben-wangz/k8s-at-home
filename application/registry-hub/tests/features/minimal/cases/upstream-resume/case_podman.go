package upstreamresume

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func verifyPodmanResume(ctx context.Context, environment testkit.CaseContext) (map[string]any, error) {
	serviceURL, err := url.Parse(environment.ClientURL)
	if err != nil || serviceURL.Scheme != "http" || serviceURL.Host == "" || serviceURL.User != nil || serviceURL.RawQuery != "" || serviceURL.Fragment != "" {
		return nil, fmt.Errorf("Registry Service URL is invalid")
	}
	root, err := os.MkdirTemp("", "registry-hub-resume-podman-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)
	authFile := filepath.Join(root, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"auths":{}}`), 0600); err != nil {
		return nil, err
	}
	image := serviceURL.Host + "/github.com/" + upstreamRepository + ":resume-podman"
	started := time.Now()
	_, err = testkit.Command(ctx, "podman", "--root", filepath.Join(root, "storage"), "--runroot", filepath.Join(root, "runroot"),
		"--storage-driver=vfs", "pull", "--authfile", authFile, "--tls-verify=false", "--platform=linux/amd64", "docker://"+image)
	if err != nil {
		return nil, fmt.Errorf("Podman pull through Registry Service failed: %w", err)
	}
	expected, digest := support.BuildResumeLayerFixture("podman")
	lines, err := fixtureLines(ctx, environment, "resume-podman", started)
	if err != nil {
		return nil, err
	}
	offsets := fixtureRangeOffsets(lines)
	if !equalOffsets(offsets, []int64{1 << 20}) || countFixtureStatus(lines, "status=206") != 1 {
		return nil, fmt.Errorf("Podman fixture did not complete through one resumed Range: offsets=%v", offsets)
	}
	return map[string]any{
		"image": image, "digest": digest, "size": len(expected),
		"rangeOffsets": offsets, "transmittedBytes": fixtureSentBytes(lines), "podmanStorageIsolated": true,
	}, nil
}
