package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type RegistryInspection struct {
	Backend        string `json:"backend"`
	FilesystemRoot string `json:"filesystemRoot"`
	S3             S3     `json:"s3"`
}

func loadRegistryInspection(cfg Config) (RegistryInspection, error) {
	inspection := RegistryInspection{Backend: "disabled", S3: cfg.S3}
	inspection.S3.Bucket, inspection.S3.Prefix = "", ""
	if raw := os.Getenv("REGISTRY_HUB_REGISTRY_INSPECTION_CONFIG"); raw != "" {
		if json.Unmarshal([]byte(raw), &inspection) != nil {
			return inspection, errors.New("registry inspection configuration is invalid")
		}
	}
	inspection.S3.AccessKeyID = os.Getenv("REGISTRY_HUB_REGISTRY_INSPECTION_ACCESS_KEY_ID")
	inspection.S3.SecretAccessKey = os.Getenv("REGISTRY_HUB_REGISTRY_INSPECTION_SECRET_ACCESS_KEY")
	return inspection, inspection.validate(cfg)
}

func (inspection RegistryInspection) validate(cfg Config) error {
	switch inspection.Backend {
	case "", "disabled":
		return nil
	case "filesystem":
		if !filepath.IsAbs(inspection.FilesystemRoot) {
			return errors.New("registry inspection filesystem root must be absolute")
		}
		if cfg.StorageBackend == "filesystem" && overlappingRoots(resolvedInspectionRoot(cfg.FilesystemRoot), resolvedInspectionRoot(inspection.FilesystemRoot), string(os.PathSeparator)) {
			return errors.New("state and registry inspection filesystem roots must not overlap")
		}
	case "s3":
		if inspection.S3.Bucket == "" {
			return errors.New("registry inspection S3 bucket is required")
		}
		if cfg.StorageBackend == "s3" && inspection.S3.Endpoint == cfg.S3.Endpoint && inspection.S3.Bucket == cfg.S3.Bucket && overlappingRoots(strings.Trim(cfg.S3.Prefix, "/"), strings.Trim(inspection.S3.Prefix, "/"), "/") {
			return errors.New("state and registry inspection S3 prefixes must not overlap")
		}
	default:
		return errors.New("registry inspection backend must be disabled, filesystem or s3")
	}
	return nil
}

func resolvedInspectionRoot(root string) string {
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		return resolved
	}
	return filepath.Clean(root)
}

func overlappingRoots(a, b, separator string) bool {
	return a == b || a == "" || b == "" || strings.HasPrefix(a, b+separator) || strings.HasPrefix(b, a+separator)
}
