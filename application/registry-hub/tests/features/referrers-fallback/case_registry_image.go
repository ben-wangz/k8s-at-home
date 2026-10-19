package referrersfallback

import (
	"context"
	"fmt"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func verifyRegistryImage(ctx context.Context, environment testkit.CaseContext) error {
	deployment, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname+"-registry")
	if err != nil {
		return err
	}
	containers := testkit.Slice(testkit.Map(testkit.Map(testkit.Map(deployment["spec"])["template"])["spec"])["containers"])
	expectedImage := expectedRegistryImageReference()
	for _, raw := range containers {
		container := testkit.Map(raw)
		if testkit.String(container["name"]) == "registry" && testkit.String(container["image"]) == expectedImage {
			return nil
		}
	}
	return fmt.Errorf("Distribution Deployment is not pinned to the expected Registry digest")
}

func expectedRegistryImageReference() string {
	repository := environmentVariable("REGISTRY_HUB_TEST_REGISTRY_IMAGE_REPOSITORY")
	if repository == "" {
		repository = expectedImageRepository
	}
	digest := environmentVariable("REGISTRY_HUB_TEST_REGISTRY_IMAGE_DIGEST")
	if digest == "" {
		digest = expectedImageDigest
	}
	return repository + "@" + digest
}
