package upstreammirror

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func requireLargeReadSource(environment testkit.CaseContext, before, after, source string) error {
	values := make([]float64, 0, 2)
	for _, phase := range []string{before, after} {
		body, err := os.ReadFile(filepath.Join(environment.OutputDir, phase+"-metrics.txt"))
		if err != nil {
			return err
		}
		value, err := largeBlobReadCounter(string(body), source)
		if err != nil {
			return err
		}
		values = append(values, value)
	}
	if delta := values[1] - values[0]; delta != 1 {
		return fmt.Errorf("expected one %s blob GET between %s and %s, observed %.0f", source, before, after, delta)
	}
	return nil
}

func largeBlobReadCounter(body, source string) (float64, error) {
	var total float64
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "registry_hub_requests_total{") || !strings.Contains(line, `operation="blob"`) || !strings.Contains(line, `method="GET"`) || !strings.Contains(line, `status="OK"`) || !strings.Contains(line, `cache_source="`+source+`"`) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return 0, fmt.Errorf("invalid blob request counter")
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return 0, err
		}
		total += value
	}
	return total, nil
}
