package testkit

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
)

var runIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,18}[a-z0-9])?$`)

func GenerateRunID() (string, error) {
	random := make([]byte, 7)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return "t" + hex.EncodeToString(random), nil
}

func ValidateRunID(id string) error {
	if len(id) > 20 || !runIDPattern.MatchString(id) {
		return fmt.Errorf("run ID must be 1-20 lowercase letters, numbers, or internal hyphens")
	}
	return nil
}

func StateRoot() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); configured != "" {
		return filepath.Join(configured, "registry-hub"), nil
	}
	current, err := user.Current()
	if err != nil {
		return "", err
	}
	return filepath.Join(current.HomeDir, ".local", "state", "registry-hub"), nil
}

func EnsurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return os.Chmod(path, 0700)
}

func WriteJSONAtomic(path string, value any) error {
	if err := EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".state-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(encoded, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func ReadJSON(path string, value any) error {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(encoded, value); err != nil {
		return fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return nil
}

func ObjectString(object map[string]any, path ...string) string {
	var value any = object
	for _, key := range path {
		value = Map(value)[key]
	}
	return String(value)
}
