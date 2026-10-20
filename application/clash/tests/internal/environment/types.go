package environment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ben-wangz/clash-tests/internal/testkit"
)

const RunLabel = "clash-console.test/run"

type Environment struct{ App, Namespace, Context string }
type Resource struct {
	Fingerprint string `json:"fingerprint,omitempty"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	UID         string `json:"uid"`
}
type Record struct {
	Mode             string               `json:"mode"`
	Tools            map[string]string    `json:"tools"`
	ID               string               `json:"id"`
	Scenario         string               `json:"scenario"`
	Namespace        string               `json:"namespace"`
	Context          string               `json:"context"`
	Release          string               `json:"release"`
	State            string               `json:"state"`
	SourceSHA        string               `json:"sourceSHA256"`
	ConfigurationSHA string               `json:"configurationSHA256"`
	FixtureImage     string               `json:"fixtureImage"`
	MihomoImage      string               `json:"mihomoImage"`
	MihomoVersion    string               `json:"mihomoVersion,omitempty"`
	ControllerURL    string               `json:"controllerURL,omitempty"`
	ProxyURL         string               `json:"proxyURL,omitempty"`
	TrafficURL       string               `json:"trafficURL,omitempty"`
	FrontendURL      string               `json:"frontendURL,omitempty"`
	Resources        []Resource           `json:"resources"`
	Plan             []string             `json:"plan"`
	Cases            []testkit.CaseResult `json:"cases"`
	Started          time.Time            `json:"started"`
	Finished         *time.Time           `json:"finished,omitempty"`
	Failure          string               `json:"failure,omitempty"`
	CleanupError     string               `json:"cleanupError,omitempty"`
}

func New(app string) Environment {
	namespace := os.Getenv("CLASH_TEST_NAMESPACE")
	if namespace == "" {
		namespace = "develop"
	}
	return Environment{App: app, Namespace: namespace, Context: os.Getenv("CLASH_TEST_CONTEXT")}
}

func (e *Environment) Kubectl(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	arguments := []string{"--context", e.Context, "--namespace", e.Namespace}
	arguments = append(arguments, args...)
	return execute(ctx, e.App, input, nil, "kubectl", arguments...)
}

func (e *Environment) Helm(ctx context.Context, args ...string) ([]byte, error) {
	arguments := []string{"--kube-context", e.Context, "--namespace", e.Namespace}
	return execute(ctx, e.App, nil, nil, "helm", append(arguments, args...)...)
}

func StateRoot() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local/state")
	}
	return filepath.Join(base, "clash/tests")
}

func CacheRoot() string {
	base, _ := os.UserCacheDir()
	return filepath.Join(base, "clash/tests")
}

func ValidID(id string) bool { return regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`).MatchString(id) }

func NewID() string {
	value := make([]byte, 5)
	_, _ = rand.Read(value)
	return "run-" + hex.EncodeToString(value)
}

func SourceIdentity(app string) (string, error) {
	var paths []string
	err := filepath.WalkDir(app, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if map[string]bool{"node_modules": true, "dist": true, "charts": true, "test-results": true, "playwright-report": true}[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("source symlinks are not allowed: %s", path)
		}
		if entry.Name() != "Chart.lock" {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	hash := sha256.New()
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		relative, _ := filepath.Rel(app, path)
		fmt.Fprintf(hash, "%s\x00%x\n", relative, sha256.Sum256(content))
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func ConfigurationIdentity(app, scenario string) (string, error) {
	content, err := os.ReadFile(filepath.Join(app, "tests/features", strings.TrimPrefix(scenario, "feature-"), "config.yaml"))
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write(content)
	for _, name := range []string{"CLASH_TEST_NAMESPACE", "CLASH_TEST_CONTEXT", "CLASH_TEST_MIHOMO_IMAGE", "CLASH_TEST_FIXTURE_IMAGE", "CLASH_TEST_IMAGE_REPOSITORY", "CLASH_TEST_INGRESS_HOST", "CLASH_TEST_INGRESS_CLASS"} {
		fmt.Fprintf(hash, "%s=%s\n", name, os.Getenv(name))
	}
	for _, name := range []string{"CLASH_TEST_TLS_CERT_FILE", "CLASH_TEST_TLS_KEY_FILE"} {
		if path := os.Getenv(name); path != "" {
			value, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(hash, "%s=%x\n", name, sha256.Sum256(value))
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
