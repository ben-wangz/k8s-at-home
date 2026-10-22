package upstreammirror

import (
	"fmt"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func verifyGraphNodeMetadata(nodes []any, digest, role, mediaType string, size int) error {
	for _, raw := range nodes {
		node := testkit.Map(raw)
		if testkit.String(node["digest"]) != digest {
			continue
		}
		if testkit.String(node["role"]) != role || testkit.String(node["mediaType"]) != mediaType || testkit.Int(node["declaredSize"]) != int64(size) || testkit.Int(node["actualSize"]) != int64(size) || node["discoveryComplete"] != true {
			return fmt.Errorf("persisted graph node %q has invalid role, metadata, size, or discovery state", digest)
		}
		return nil
	}
	return fmt.Errorf("persisted graph node %q is missing", digest)
}
