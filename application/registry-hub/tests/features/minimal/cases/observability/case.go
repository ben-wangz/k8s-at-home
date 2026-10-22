package observability

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"regexp"
	"strings"
)

//go:embed case.json
var metadata embed.FS

var metricLabelPattern = regexp.MustCompile(`\{([^}]*)\}`)

var filesystemS3ReplayMetrics = map[string]struct{}{
	"registry_hub_s3_replay_memory_bytes":   {},
	"registry_hub_s3_replay_disk_bytes":     {},
	"registry_hub_s3_replay_active_sources": {},
}

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "observability")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	logs, err := backendLogs(ctx, environment)
	if err != nil {
		return err
	}
	logLines, warningSeen, err := validateLogs(logs)
	if err != nil {
		return err
	}
	metrics, err := readMetrics(ctx, environment)
	if err != nil {
		return err
	}
	metricNames, err := validateMetrics(metrics)
	if err != nil {
		return err
	}
	if err := validateDisabledGCMetrics(metrics); err != nil {
		return err
	}
	if err := validateDisabledFeatureMetrics(metrics); err != nil {
		return err
	}
	if err := validateFilesystemS3ReplayMetrics(metrics); err != nil {
		return err
	}
	secretWords, err := secretValues(ctx, environment)
	if err != nil {
		return err
	}
	adminResponse, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks?limit=1", nil, nil)
	if err != nil {
		return err
	}
	adminBody, err := testkit.ReadBody(adminResponse, 2<<20)
	if err != nil {
		return err
	}
	template, err := testkit.Command(ctx, "helm", "template", environment.Release, environment.ChartDir, "--namespace", environment.Namespace, "--values", environment.BaseValues, "--values", environment.ValuesFile)
	if err != nil {
		return err
	}
	observed := string(logs) + string(metrics) + string(adminBody) + string(template.Stdout)
	for _, word := range secretWords {
		if word != "" && strings.Contains(observed, word) {
			return fmt.Errorf("observability output contains secret material")
		}
	}
	if logLines == 0 {
		return fmt.Errorf("backend emitted no JSON log lines")
	}
	return support.WriteResult(environment, map[string]any{"logLines": logLines, "noAuthWarning": warningSeen, "metricNames": metricNames, "disabledGCMetricState": "no execution samples", "disabledFeatureMetrics": "no samples", "secretScan": "clear"})
}

func backendLogs(ctx context.Context, environment testkit.CaseContext) ([]byte, error) {
	result, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", "deployment/"+environment.Fullname, "-c", "backend", "--tail=2000")
	if err != nil {
		return nil, err
	}
	return result.Stdout, nil
}

func validateLogs(logs []byte) (int, bool, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(logs)))
	count := 0
	warning := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			return 0, false, fmt.Errorf("backend log is not JSON: %w", err)
		}
		if _, ok := record["level"]; !ok {
			return 0, false, fmt.Errorf("backend log has no level field")
		}
		if strings.Contains(strings.ToLower(line), "client authentication is disabled") {
			warning = true
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		return 0, false, err
	}
	if !warning {
		return count, false, fmt.Errorf("no-auth startup warning was not found in backend logs")
	}
	return count, warning, nil
}

func readMetrics(ctx context.Context, environment testkit.CaseContext) ([]byte, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/metrics", nil, nil)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("metrics returned %s", response.Status)
	}
	return testkit.ReadBody(response, 4<<20)
}

func validateMetrics(metrics []byte) ([]string, error) {
	required := []string{
		"registry_hub_requests_total", "registry_hub_request_duration_seconds", "registry_hub_storage_operations_total",
		"registry_hub_tasks", "registry_hub_tasks_by_kind_state", "registry_hub_tasks_runnable", "registry_hub_tasks_waiting",
		"registry_hub_task_waiting", "registry_hub_task_retry_attempts", "registry_hub_task_cancellations",
		"registry_hub_solidification_recovery_unknown", "registry_hub_gc_blockers", "registry_hub_gc_gate",
		"registry_hub_download_partial_reserved_bytes", "registry_hub_download_partial_sessions", "registry_hub_s3_replay_disk_bytes",
		"registry_hub_events_total", "registry_hub_object_leases",
		"registry_hub_cache_objects", "registry_hub_cache_bytes", "registry_hub_cache_staging_objects",
		"registry_hub_cache_orphan_objects", "registry_hub_cache_cleanup_pending_objects",
		"registry_hub_distribution_response_validation_failures_total", "registry_hub_referrer_repair_pending",
	}
	for _, name := range required {
		if !bytesContainsLine(metrics, name) {
			return nil, fmt.Errorf("metrics do not expose %s", name)
		}
	}
	seen := make(map[string]struct{})
	scanner := bufio.NewScanner(strings.NewReader(string(metrics)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		name := line
		if index := strings.IndexAny(name, " {"); index >= 0 {
			name = name[:index]
		}
		seen[name] = struct{}{}
		for _, labels := range metricLabelPattern.FindAllStringSubmatch(line, -1) {
			for _, label := range strings.Split(labels[1], ",") {
				key := strings.TrimSpace(strings.SplitN(label, "=", 2)[0])
				if key == "repository" || key == "tag" || key == "digest" || key == "token" || key == "url" || key == "username" {
					return nil, fmt.Errorf("metric has high-cardinality or sensitive label %s", key)
				}
			}
		}
	}
	result := make([]string, 0, len(seen))
	for name := range seen {
		result = append(result, name)
	}
	return result, scanner.Err()
}
