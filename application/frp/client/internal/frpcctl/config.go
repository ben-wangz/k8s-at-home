package frpcctl

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

const (
	defaultMonitorConfigPath      = "/etc/frp-monitor/config.json"
	defaultWebhookCredentialsPath = "/etc/frp-monitor/webhook.json"
	defaultStatePath              = "/var/lib/frp-monitor/state.json"
	defaultHostKeyPath            = "/etc/ssh/ssh_host_ed25519_key.pub"
	defaultSSHKeyscanPath         = "/usr/bin/ssh-keyscan"
)

type MonitorConfig struct {
	TargetHost                  string `json:"targetHost"`
	TargetPort                  int    `json:"targetPort"`
	CheckTimeoutSeconds         int    `json:"checkTimeoutSeconds"`
	FailureThresholdSeconds     int    `json:"failureThresholdSeconds"`
	NotificationIntervalSeconds int    `json:"notificationIntervalSeconds"`
	HostKeyPath                 string `json:"hostKeyPath"`
	SSHKeyscanPath              string `json:"sshKeyscanPath"`
	StatePath                   string `json:"statePath"`
	WebhookCredentialsPath      string `json:"webhookCredentialsPath"`
}

type WebhookCredentials struct {
	Type   string `json:"type"`
	URL    string `json:"url"`
	Secret string `json:"secret"`
}

func loadMonitorConfig(path string) (MonitorConfig, error) {
	var config MonitorConfig
	if err := readJSON(path, &config); err != nil {
		return config, err
	}
	applyMonitorDefaults(&config)
	if err := validateMonitorConfig(config); err != nil {
		return config, fmt.Errorf("invalid monitor config: %w", err)
	}
	return config, nil
}

func applyMonitorDefaults(config *MonitorConfig) {
	if config.CheckTimeoutSeconds == 0 {
		config.CheckTimeoutSeconds = 10
	}
	if config.FailureThresholdSeconds == 0 {
		config.FailureThresholdSeconds = 3600
	}
	if config.NotificationIntervalSeconds == 0 {
		config.NotificationIntervalSeconds = 21600
	}
	if config.HostKeyPath == "" {
		config.HostKeyPath = defaultHostKeyPath
	}
	if config.SSHKeyscanPath == "" {
		config.SSHKeyscanPath = defaultSSHKeyscanPath
	}
	if config.StatePath == "" {
		config.StatePath = defaultStatePath
	}
	if config.WebhookCredentialsPath == "" {
		config.WebhookCredentialsPath = defaultWebhookCredentialsPath
	}
}

func validateMonitorConfig(config MonitorConfig) error {
	if config.TargetHost == "" || strings.HasPrefix(config.TargetHost, "-") || strings.ContainsAny(config.TargetHost, " \t\r\n") {
		return fmt.Errorf("targetHost must be a non-empty hostname or address")
	}
	if config.TargetPort < 1 || config.TargetPort > 65535 {
		return fmt.Errorf("targetPort must be between 1 and 65535")
	}
	if config.CheckTimeoutSeconds < 1 || config.CheckTimeoutSeconds > 300 {
		return fmt.Errorf("checkTimeoutSeconds must be between 1 and 300")
	}
	if config.FailureThresholdSeconds < 1 {
		return fmt.Errorf("failureThresholdSeconds must be positive")
	}
	if config.NotificationIntervalSeconds < 1 {
		return fmt.Errorf("notificationIntervalSeconds must be positive")
	}
	for name, path := range map[string]string{
		"hostKeyPath":            config.HostKeyPath,
		"sshKeyscanPath":         config.SSHKeyscanPath,
		"statePath":              config.StatePath,
		"webhookCredentialsPath": config.WebhookCredentialsPath,
	} {
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf("%s must be an absolute path", name)
		}
	}
	return nil
}

func loadWebhookCredentials(path string) (WebhookCredentials, error) {
	var credentials WebhookCredentials
	if err := readJSON(path, &credentials); err != nil {
		return credentials, err
	}
	if err := validateWebhookCredentials(credentials); err != nil {
		return credentials, fmt.Errorf("invalid webhook credentials: %w", err)
	}
	return credentials, nil
}

func validateWebhookCredentials(credentials WebhookCredentials) error {
	if credentials.Type != "dingtalk" {
		return fmt.Errorf("type must be dingtalk")
	}
	parsed, err := url.Parse(credentials.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() != "oapi.dingtalk.com" {
		return fmt.Errorf("url must be an HTTPS DingTalk robot endpoint")
	}
	if parsed.Query().Get("access_token") == "" {
		return fmt.Errorf("url must contain an access_token query parameter")
	}
	if !strings.HasPrefix(credentials.Secret, "SEC") {
		return fmt.Errorf("secret must start with SEC")
	}
	return nil
}

func readJSON(path string, destination any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, destination); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}
