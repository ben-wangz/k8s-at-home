package gc

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func TestInternalDistributionRepositoryMapsQualifiedRepository(t *testing.T) {
	tests := []struct {
		repository string
		want       string
	}{
		{repository: "github.com/registry-hub-test/fixture", want: "egithub_2ecom/registry-hub-test/fixture"},
		{repository: "ghcr.io/team/image", want: "eghcr_2eio/team/image"},
		{repository: "team/image", want: "team/image"},
	}
	for _, test := range tests {
		t.Run(test.repository, func(t *testing.T) {
			if got := internalDistributionRepository(test.repository); got != test.want {
				t.Fatalf("internalDistributionRepository(%q) = %q, want %q", test.repository, got, test.want)
			}
		})
	}
}

func TestDirectDistributionProbeSkipsDeleteWhenBlobIsMissing(t *testing.T) {
	manifest, err := directDistributionProbeManifest(
		testkit.CaseContext{Namespace: "develop", RunID: "run-id", Fullname: "rh-test"},
		map[string]any{"app": "registry-hub"}, "probe", "curl:latest", "registry", "10.0.0.1", "node-1",
		"egithub_2ecom/team/image", "sha256:"+strings.Repeat("a", 64),
	)
	if err != nil {
		t.Fatal(err)
	}
	var pod struct {
		Spec struct {
			Containers []struct {
				Args []string `json:"args"`
			} `json:"containers"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(manifest, &pod); err != nil {
		t.Fatal(err)
	}
	if len(pod.Spec.Containers) != 1 || len(pod.Spec.Containers[0].Args) != 1 {
		t.Fatalf("probe container args = %#v", pod.Spec.Containers)
	}
	script := pod.Spec.Containers[0].Args[0]
	if !strings.Contains(script, `if [ "$before" = '200/0' ]; then`) || !strings.Contains(script, "deleted=SKIPPED") {
		t.Fatalf("probe script does not gate DELETE on a successful pre-read: %s", script)
	}
}
