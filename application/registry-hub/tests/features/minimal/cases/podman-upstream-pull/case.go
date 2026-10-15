package podmanpull

import (
	"context"
	"embed"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

const (
	upstreamImage               = "ghcr.nju.edu.cn/oras-project/oras"
	upstreamTag                 = "v1.2.3"
	upstreamIndexDigest         = "sha256:63266a046d1cf5ebebb1461733ed7548148f122e0d422096e177cfa70b521cb1"
	upstreamAMD64ManifestDigest = "sha256:60acaf26092935c452436c865ede9ad8dcd181b6feff961eac8af05311e64ad8"
)

type pullReference struct {
	name      string
	reference string
}

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "podman-upstream-pull")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	serviceURL, err := url.Parse(environment.ClientURL)
	if err != nil || serviceURL.Scheme != "http" || serviceURL.Host == "" || serviceURL.User != nil ||
		serviceURL.RawQuery != "" || serviceURL.Fragment != "" || serviceURL.Path != "" && serviceURL.Path != "/" {
		return fmt.Errorf("client Registry Service URL is invalid")
	}

	podmanRoot, err := os.MkdirTemp("", "rhpull-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(podmanRoot)
	authFile := filepath.Join(podmanRoot, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"auths":{}}`), 0600); err != nil {
		return err
	}

	serviceImage := serviceURL.Host + "/" + upstreamImage
	pulls := []pullReference{
		{name: "tag", reference: serviceImage + ":" + upstreamTag},
		{name: "index-digest", reference: serviceImage + "@" + upstreamIndexDigest},
		{name: "platform-digest", reference: serviceImage + "@" + upstreamAMD64ManifestDigest},
	}
	results := make([]map[string]string, 0, len(pulls))
	for index, pull := range pulls {
		storageRoot, err := os.MkdirTemp(podmanRoot, pull.name+"-storage-")
		if err != nil {
			return err
		}
		runRoot := filepath.Join(podmanRoot, fmt.Sprintf("r%d", index))
		storageRoot = filepath.Join(storageRoot, "store")
		_, err = testkit.Command(ctx, "podman",
			"--root", storageRoot,
			"--runroot", runRoot,
			"--storage-driver=vfs",
			"pull",
			"--authfile", authFile,
			"--tls-verify=false",
			"--platform=linux/amd64",
			"docker://"+pull.reference,
		)
		if err != nil {
			return fmt.Errorf("Podman pull by %s %s: %w", pull.name, pull.reference, err)
		}
		results = append(results, map[string]string{"mode": pull.name, "reference": pull.reference})
	}

	return support.WriteResult(environment, map[string]any{
		"image":                  upstreamImage,
		"tag":                    upstreamTag,
		"indexDigest":            upstreamIndexDigest,
		"platform":               "linux/amd64",
		"platformManifestDigest": upstreamAMD64ManifestDigest,
		"pulls":                  results,
		"podmanStorageIsolated":  true,
	})
}
