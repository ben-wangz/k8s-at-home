package config

import "testing"

func TestRegistryInspectionS3ConfigurationLoadsDedicatedCredentials(t *testing.T) {
	t.Setenv("REGISTRY_HUB_REGISTRY_INSPECTION_CONFIG", `{"backend":"s3","s3":{"endpoint":"http://127.0.0.1:9000","region":"inspection-region","bucket":"registry-inspection","prefix":"distribution","usePathStyle":true,"allowHTTP":true}}`)
	t.Setenv("REGISTRY_HUB_REGISTRY_INSPECTION_ACCESS_KEY_ID", "inspection-access")
	t.Setenv("REGISTRY_HUB_REGISTRY_INSPECTION_SECRET_ACCESS_KEY", "inspection-secret")

	inspection, err := loadRegistryInspection(Config{StorageBackend: "filesystem", FilesystemRoot: t.TempDir(), S3: S3{Region: "state-region", Bucket: "state"}})
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Backend != "s3" || inspection.S3.Endpoint != "http://127.0.0.1:9000" || inspection.S3.Region != "inspection-region" || inspection.S3.Bucket != "registry-inspection" || inspection.S3.Prefix != "distribution" || !inspection.S3.UsePathStyle || !inspection.S3.AllowHTTP {
		t.Fatalf("unexpected registry inspection configuration: %+v", inspection)
	}
	if inspection.S3.AccessKeyID != "inspection-access" || inspection.S3.SecretAccessKey != "inspection-secret" {
		t.Fatal("registry inspection credentials were not loaded from their dedicated environment variables")
	}
}
