package runner

import (
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func containsResource(resources []testkit.ResourceIdentity, expected testkit.ResourceIdentity) bool {
	for _, resource := range resources {
		if resource.Kind == expected.Kind && resource.Name == expected.Name && resource.UID == expected.UID {
			return true
		}
	}
	return false
}
