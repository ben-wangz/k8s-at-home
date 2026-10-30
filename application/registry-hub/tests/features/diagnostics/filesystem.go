package diagnostics

import (
	"context"
	"fmt"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func verifyRegistryFiles(ctx context.Context, environment testkit.CaseContext, snapshot report) error {
	const command = `find /var/lib/registry -type f -exec stat -c %s {} \; | awk '{bytes+=$1; count++} END {print count+0, bytes+0}'`
	result, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "exec", "deployment/"+environment.Fullname+"-registry", "-c", "registry", "--", "sh", "-c", command)
	if err != nil {
		return err
	}
	var count, bytes int64
	if _, err := fmt.Sscan(string(result.Stdout), &count, &bytes); err != nil {
		return fmt.Errorf("decode independent registry file totals: %w", err)
	}
	registry := snapshot.Domains["registry"]
	if registry.Objects == nil || registry.ObjectBytes == nil || count != *registry.Objects || bytes != *registry.ObjectBytes {
		return fmt.Errorf("registry physical report differs from independent file metadata: files=%d bytes=%d", count, bytes)
	}
	return nil
}
