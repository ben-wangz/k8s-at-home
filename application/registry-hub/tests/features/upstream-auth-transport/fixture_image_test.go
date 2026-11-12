package upstreamauthtransport

import (
	"strings"
	"testing"
)

func TestFixtureSourcesExcludeTests(t *testing.T) {
	sources, err := FixtureSources()
	if err != nil {
		t.Fatal(err)
	}
	if sources["seed.go"] == "" {
		t.Fatal("compiled fixture does not include task seeding")
	}
	for name := range sources {
		if strings.HasSuffix(name, "_test.go") {
			t.Fatalf("test source included in fixture image: %s", name)
		}
	}
}

func TestProxyImageTagTracksSourcesRecipeAndPlatform(t *testing.T) {
	a := map[string]string{"main.go": "main", "runtime.go": "runtime"}
	b := map[string]string{"runtime.go": "runtime", "main.go": "main"}
	tag := proxyImageTag(a, []byte("recipe"), "linux/amd64")
	if tag != proxyImageTag(b, []byte("recipe"), "linux/amd64") {
		t.Fatal("source iteration order changed the fixture image")
	}
	b["runtime.go"] = "changed"
	for _, other := range []string{
		proxyImageTag(b, []byte("recipe"), "linux/amd64"),
		proxyImageTag(a, []byte("new recipe"), "linux/amd64"),
		proxyImageTag(a, []byte("recipe"), "linux/arm64"),
	} {
		if tag == other {
			t.Fatal("fixture image reused incompatible build inputs")
		}
	}
}
