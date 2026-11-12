package installation

import (
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func expectedPersistentClaims(values map[string]any, fullname string) (map[string]string, error) {
	backends := map[string]string{
		"backend":  testkit.String(testkit.Map(values["storage"])["backend"]),
		"registry": testkit.String(testkit.Map(testkit.Map(values["registry"])["storage"])["type"]),
	}
	expected := map[string]string{}
	for component, backend := range backends {
		switch backend {
		case "filesystem":
			size := "20Gi"
			if component == "registry" {
				size = "100Gi"
			}
			expected[fullname+"-"+component] = size
		case "s3":
		default:
			return nil, fmt.Errorf("unsupported %s storage backend %q", component, backend)
		}
	}
	return expected, nil
}
