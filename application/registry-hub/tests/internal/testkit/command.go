package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

type CommandResult struct {
	Stdout []byte
	Stderr []byte
}

func Command(ctx context.Context, name string, args ...string) (CommandResult, error) {
	return runCommand(ctx, "", name, args...)
}

func CommandDir(ctx context.Context, directory, name string, args ...string) (CommandResult, error) {
	return runCommand(ctx, directory, name, args...)
}

func runCommand(ctx context.Context, directory, name string, args ...string) (CommandResult, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = directory
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if len(message) > 1200 {
			message = message[:1200]
		}
		if message != "" {
			return CommandResult{}, fmt.Errorf("%s failed: %w: %s", name, err, message)
		}
		return CommandResult{}, fmt.Errorf("%s failed: %w", name, err)
	}
	return CommandResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, nil
}

func CommandInput(ctx context.Context, input []byte, name string, args ...string) (CommandResult, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if len(message) > 1200 {
			message = message[:1200]
		}
		if message != "" {
			return CommandResult{}, fmt.Errorf("%s failed: %w: %s", name, err, message)
		}
		return CommandResult{}, fmt.Errorf("%s failed: %w", name, err)
	}
	return CommandResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, nil
}

func KubectlJSON(ctx context.Context, namespace string, args ...string) (map[string]any, error) {
	commandArgs := append([]string{"-n", namespace}, args...)
	commandArgs = append(commandArgs, "-o", "json")
	result, err := Command(ctx, "kubectl", commandArgs...)
	if err != nil {
		return nil, err
	}
	var object map[string]any
	if err := json.Unmarshal(result.Stdout, &object); err != nil {
		return nil, fmt.Errorf("decode kubectl JSON: %w", err)
	}
	return object, nil
}

func KubectlGet(ctx context.Context, namespace, resource, name string) (map[string]any, error) {
	return KubectlJSON(ctx, namespace, "get", resource, name)
}

func Map(value any) map[string]any {
	result, _ := value.(map[string]any)
	if result == nil {
		return map[string]any{}
	}
	return result
}

func Slice(value any) []any {
	result, _ := value.([]any)
	return result
}

func String(value any) string {
	result, _ := value.(string)
	return result
}

func Int(value any) int64 {
	switch number := value.(type) {
	case float64:
		return int64(number)
	case int64:
		return number
	default:
		return 0
	}
}

func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "notfound") || strings.Contains(message, "not found")
}

func IsAlreadyExists(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "alreadyexists")
}
