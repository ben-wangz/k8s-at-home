package observability

import (
	"context"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"strconv"
	"strings"
)

func validateDisabledGCMetrics(metrics []byte) error {
	for _, line := range strings.Split(string(metrics), "\n") {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		name := line
		if index := strings.IndexAny(name, " {"); index >= 0 {
			name = name[:index]
		}
		if name != "registry_hub_gc_runs_total" && name != "registry_hub_gc_duration_seconds_count" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return fmt.Errorf("GC metric sample is malformed")
		}
		value := strings.TrimSpace(fields[len(fields)-1])
		if value != "0" && value != "0.0" {
			return fmt.Errorf("disabled GC exposed an execution metric sample")
		}
	}
	return nil
}

func validateDisabledFeatureMetrics(metrics []byte) error {
	features := []string{"catalog", "storage_redirect"}
	for _, line := range strings.Split(string(metrics), "\n") {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		name := line
		if index := strings.IndexAny(name, " {"); index >= 0 {
			name = name[:index]
		}
		if strings.Contains(strings.ToLower(name), "s3") {
			if _, ok := filesystemS3ReplayMetrics[name]; !ok {
				return fmt.Errorf("disabled s3 feature exposed unexpected metric sample %s", name)
			}
			continue
		}
		for _, feature := range features {
			if strings.Contains(strings.ToLower(name), feature) {
				return fmt.Errorf("disabled %s feature exposed metric sample %s", feature, name)
			}
		}
	}
	return nil
}

func validateFilesystemS3ReplayMetrics(metrics []byte) error {
	seen := make(map[string]struct{}, len(filesystemS3ReplayMetrics))
	for _, line := range strings.Split(string(metrics), "\n") {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		name := fields[0]
		if index := strings.IndexAny(name, " {"); index >= 0 {
			name = name[:index]
		}
		if _, ok := filesystemS3ReplayMetrics[name]; !ok {
			continue
		}
		if len(fields) < 2 {
			return fmt.Errorf("filesystem S3 replay metric sample %s is malformed", name)
		}
		value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil || value != 0 {
			return fmt.Errorf("filesystem S3 replay metric %s is %q, want zero", name, fields[len(fields)-1])
		}
		seen[name] = struct{}{}
	}
	for name := range filesystemS3ReplayMetrics {
		if _, ok := seen[name]; !ok {
			return fmt.Errorf("filesystem S3 replay metric sample %s is missing", name)
		}
	}
	return nil
}

func secretValues(ctx context.Context, environment testkit.CaseContext) ([]string, error) {
	words := make([]string, 0, 8)
	for _, name := range []string{environment.Fullname + "-registry-internal", environment.Fullname + "-admin-auth"} {
		_, data, err := support.GetSecret(ctx, environment.Namespace, name)
		if err != nil {
			return nil, err
		}
		for key, value := range data {
			if key == "ca.crt" || key == "tls.crt" {
				continue
			}
			words = append(words, string(value))
		}
	}
	return words, nil
}

func bytesContainsLine(content []byte, value string) bool {
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, value) || strings.HasPrefix(line, "# HELP "+value) || strings.HasPrefix(line, "# TYPE "+value) {
			return true
		}
	}
	return false
}
