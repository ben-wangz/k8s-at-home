package gc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

const podmanDebugOutputLimit = 12 << 10

func podmanPullPinnedImage(ctx context.Context, environment testkit.CaseContext, credential *testkit.BasicCredential, repository, digest string) (map[string]any, error) {
	if credential == nil || credential.Username == "" || credential.Password == "" || !strings.HasPrefix(digest, "sha256:") {
		return nil, fmt.Errorf("pinned repull lacks a writer credential or manifest digest")
	}
	registryURL, err := url.Parse(environment.ClientURL)
	if err != nil || registryURL.Host == "" || registryURL.User != nil || registryURL.RawQuery != "" || registryURL.Fragment != "" {
		return nil, fmt.Errorf("Registry Service URL is invalid for Podman repull")
	}
	root, err := os.MkdirTemp("", "registry-hub-gc-repull-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)
	auth := map[string]any{"auths": map[string]any{
		registryURL.Host: map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(credential.Username + ":" + credential.Password))},
	}}
	encodedAuth, err := json.Marshal(auth)
	if err != nil {
		return nil, err
	}
	authFile := filepath.Join(root, "auth.json")
	if err := os.WriteFile(authFile, encodedAuth, 0600); err != nil {
		return nil, err
	}
	imagePath := strings.Trim(registryURL.EscapedPath(), "/")
	if imagePath != "" {
		imagePath += "/"
	}
	image := registryURL.Host + "/" + imagePath + repository + "@" + digest
	command := exec.CommandContext(ctx, "podman", "--log-level=debug", "--root", filepath.Join(root, "storage"), "--runroot", filepath.Join(root, "runroot"),
		"--storage-driver=vfs", "pull", "--authfile", authFile, "--tls-verify=false", "--platform=linux/amd64", "docker://"+image)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err = command.Run()
	if err != nil {
		output := strings.TrimSpace(stderr.String())
		if stdout.Len() > 0 {
			output += "\nstdout:\n" + stdout.String()
		}
		output = boundedPodmanDebugOutput(output, credential)
		if output != "" {
			return nil, fmt.Errorf("clean Podman pull of %s failed: %w; Podman debug output (tail):\n%s", image, err, output)
		}
		return nil, fmt.Errorf("clean Podman pull of %s failed: %w", image, err)
	}
	result := map[string]any{"image": image, "manifestDigest": digest, "storageIsolated": true}
	output := strings.TrimSpace(stderr.String())
	if stdout.Len() > 0 {
		output += "\nstdout:\n" + stdout.String()
	}
	if output = boundedPodmanDebugOutput(output, credential); output != "" {
		result["debugOutputTail"] = output
	}
	return result, nil
}

func boundedPodmanDebugOutput(output string, credential *testkit.BasicCredential) string {
	if credential != nil {
		for _, secret := range []string{
			credential.Username,
			credential.Password,
			credential.Username + ":" + credential.Password,
			base64.StdEncoding.EncodeToString([]byte(credential.Username + ":" + credential.Password)),
		} {
			if secret != "" {
				output = strings.ReplaceAll(output, secret, "[redacted]")
			}
		}
	}
	if len(output) > podmanDebugOutputLimit {
		start := len(output) - podmanDebugOutputLimit
		for start < len(output) && !utf8.RuneStart(output[start]) {
			start++
		}
		output = output[start:]
	}
	return strings.TrimSpace(output)
}
