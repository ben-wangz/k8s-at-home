package frpcctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type monitorState struct {
	FailureStartedAt   *int64 `json:"failureStartedAt"`
	LastNotificationAt *int64 `json:"lastNotificationAt"`
}

type probeFailureError struct {
	detail string
}

func (failure probeFailureError) Error() string {
	return failure.detail
}

func monitorOnce(config MonitorConfig, output io.Writer) error {
	now := time.Now()
	state, err := loadMonitorState(config.StatePath)
	if err != nil {
		return err
	}
	healthy, detail := probePublicSSH(config)
	if healthy {
		if state.FailureStartedAt != nil {
			duration := now.Sub(time.Unix(*state.FailureStartedAt, 0))
			fmt.Fprintf(output, "recovered silently after %.0f minutes: %s\n", duration.Minutes(), detail)
		} else {
			fmt.Fprintf(output, "healthy: %s\n", detail)
		}
		state.FailureStartedAt = nil
		return saveMonitorState(config.StatePath, state)
	}

	nowEpoch := now.Unix()
	if state.FailureStartedAt == nil || *state.FailureStartedAt > nowEpoch {
		state.FailureStartedAt = &nowEpoch
	}
	failureDuration := now.Sub(time.Unix(*state.FailureStartedAt, 0))
	notificationDue := failureDuration >= time.Duration(config.FailureThresholdSeconds)*time.Second &&
		(state.LastNotificationAt == nil || now.Sub(time.Unix(*state.LastNotificationAt, 0)) >= time.Duration(config.NotificationIntervalSeconds)*time.Second)

	if notificationDue {
		credentials, loadErr := loadWebhookCredentials(config.WebhookCredentialsPath)
		if loadErr != nil {
			_ = saveMonitorState(config.StatePath, state)
			return loadErr
		}
		message := buildAlertMessage(config, now, *state.FailureStartedAt, failureDuration, detail)
		if sendErr := sendDingTalk(credentials, message, config.CheckTimeoutSeconds); sendErr != nil {
			_ = saveMonitorState(config.StatePath, state)
			return fmt.Errorf("send failure notification: %w", sendErr)
		}
		state.LastNotificationAt = &nowEpoch
		fmt.Fprintf(output, "failure notification sent after %.0f minutes: %s\n", failureDuration.Minutes(), detail)
	} else if failureDuration < time.Duration(config.FailureThresholdSeconds)*time.Second {
		remaining := time.Duration(config.FailureThresholdSeconds)*time.Second - failureDuration
		fmt.Fprintf(output, "failure pending: %.0f minutes, notification eligible in %.0f minutes: %s\n", failureDuration.Minutes(), remaining.Minutes(), detail)
	} else {
		remaining := time.Duration(config.NotificationIntervalSeconds)*time.Second - now.Sub(time.Unix(*state.LastNotificationAt, 0))
		fmt.Fprintf(output, "failure rate-limited for another %.1f hours: %s\n", max(0, remaining.Hours()), detail)
	}
	if err := saveMonitorState(config.StatePath, state); err != nil {
		return err
	}
	return probeFailureError{detail: detail}
}

func probePublicSSH(config MonitorConfig) (bool, string) {
	expected, err := readPublicHostKey(config.HostKeyPath)
	if err != nil {
		return false, err.Error()
	}
	timeout := time.Duration(config.CheckTimeoutSeconds) * time.Second
	contextWithTimeout, cancel := context.WithTimeout(context.Background(), timeout+5*time.Second)
	defer cancel()
	command := exec.CommandContext(
		contextWithTimeout,
		config.SSHKeyscanPath,
		"-T", strconv.Itoa(config.CheckTimeoutSeconds),
		"-p", strconv.Itoa(config.TargetPort),
		"-t", "ed25519",
		config.TargetHost,
	)
	stdout, err := command.Output()
	if contextWithTimeout.Err() == context.DeadlineExceeded {
		return false, fmt.Sprintf("SSH key scan timed out after %d seconds", config.CheckTimeoutSeconds)
	}
	for _, line := range strings.Split(string(stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[1] == "ssh-ed25519" && fields[1]+" "+fields[2] == expected {
			return true, "public SSH host key matches the local host"
		}
	}
	if len(stdout) > 0 {
		return false, "public SSH host key does not match the local host"
	}
	if exitError, ok := err.(*exec.ExitError); ok {
		detail := strings.Join(strings.Fields(string(exitError.Stderr)), " ")
		if detail != "" {
			return false, truncate(detail, 300)
		}
	}
	if err != nil {
		return false, truncate(err.Error(), 300)
	}
	return false, "no SSH host key returned"
}

func readPublicHostKey(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read local SSH host key: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 || fields[0] != "ssh-ed25519" {
		return "", fmt.Errorf("invalid local ED25519 host key: %s", path)
	}
	return fields[0] + " " + fields[1], nil
}

func loadMonitorState(path string) (monitorState, error) {
	var state monitorState
	err := readJSON(path, &state)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	return state, nil
}

func saveMonitorState(path string, state monitorState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode monitor state: %w", err)
	}
	return atomicWrite(path, append(data, '\n'), 0o600)
}

func buildAlertMessage(config MonitorConfig, now time.Time, failureStartedAt int64, duration time.Duration, detail string) string {
	return strings.Join([]string{
		"[frp SSH tunnel alert]",
		fmt.Sprintf("probe: %s:%d", config.TargetHost, config.TargetPort),
		"monitor: independent systemd end-to-end probe",
		fmt.Sprintf("continuous failure: %.0f minutes", duration.Minutes()),
		fmt.Sprintf("failure started: %s", time.Unix(failureStartedAt, 0).Local().Format(time.RFC3339)),
		fmt.Sprintf("checked at: %s", now.Local().Format(time.RFC3339)),
		fmt.Sprintf("reason: %s", detail),
		fmt.Sprintf("notification limit: at most once every %.0f hours", (time.Duration(config.NotificationIntervalSeconds) * time.Second).Hours()),
	}, "\n")
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
