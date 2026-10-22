package observability

import "testing"

func TestFilesystemS3ReplayMetricsAreAllowedAndZero(t *testing.T) {
	metrics := []byte("" +
		"registry_hub_s3_replay_memory_bytes 0\n" +
		"registry_hub_s3_replay_disk_bytes 0\n" +
		"registry_hub_s3_replay_active_sources 0\n")
	if err := validateDisabledFeatureMetrics(metrics); err != nil {
		t.Fatalf("validateDisabledFeatureMetrics() error = %v", err)
	}
	if err := validateFilesystemS3ReplayMetrics(metrics); err != nil {
		t.Fatalf("validateFilesystemS3ReplayMetrics() error = %v", err)
	}
}

func TestFilesystemS3ReplayMetricsRejectNonzeroValue(t *testing.T) {
	metrics := []byte("" +
		"registry_hub_s3_replay_memory_bytes 0\n" +
		"registry_hub_s3_replay_disk_bytes 0\n" +
		"registry_hub_s3_replay_active_sources 1\n")
	if err := validateFilesystemS3ReplayMetrics(metrics); err == nil {
		t.Fatal("validateFilesystemS3ReplayMetrics() accepted a nonzero replay gauge")
	}
}

func TestDisabledFeatureMetricsRejectUnexpectedS3Metric(t *testing.T) {
	metrics := []byte("registry_hub_s3_upload_retries_total 0\n")
	if err := validateDisabledFeatureMetrics(metrics); err == nil {
		t.Fatal("validateDisabledFeatureMetrics() accepted an unexpected S3 metric")
	}
}
