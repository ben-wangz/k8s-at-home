package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	ListenAddr   string
	Database     DatabaseConfig
	ArtifactsDir string
	AuthMode     string
}

type DatabaseConfig struct {
	Driver     string
	DSN        string
	DataDir    string
	ReadyQuery string
}

func Load() Config {
	dataDir := envOrDefault("ATM_DATA_DIR", "/var/lib/agent-task-manager")
	driver := strings.ToLower(envOrDefault("ATM_DATABASE_DRIVER", "sqlite"))
	dsn := strings.TrimSpace(os.Getenv("ATM_DATABASE_DSN"))
	if dsn == "" {
		dsn = defaultDSN(driver, dataDir)
	}

	return Config{
		ListenAddr: envOrDefault("ATM_API_ADDR", ":8080"),
		Database: DatabaseConfig{
			Driver:     driver,
			DSN:        dsn,
			DataDir:    dataDir,
			ReadyQuery: envOrDefault("ATM_DATABASE_READY_QUERY", "SELECT 1"),
		},
		ArtifactsDir: envOrDefault("ATM_ARTIFACTS_DIR", filepath.Join(dataDir, "artifacts")),
		AuthMode:     strings.ToLower(envOrDefault("ATM_AUTH_MODE", "disabled")),
	}
}

func defaultDSN(driver, dataDir string) string {
	if driver == "postgres" {
		return "postgres://agent-task-manager:agent-task-manager@127.0.0.1:5432/agent_task_manager?sslmode=disable"
	}

	sqlitePath := filepath.Join(dataDir, "agent-task-manager.sqlite")
	return fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", sqlitePath)
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
