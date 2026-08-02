package frpcctl

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

const (
	installedBinaryPath = "/usr/local/sbin/frpcctl"
	frpcConfigPath      = "/etc/frp/frpc.toml"
	quadletPath         = "/etc/containers/systemd/frpc.container"
	monitorServicePath  = "/etc/systemd/system/frp-monitor.service"
	monitorTimerPath    = "/etc/systemd/system/frp-monitor.timer"
	quadletBinaryPath   = "/usr/libexec/podman/quadlet"
)

var credentialLiteralPattern = regexp.MustCompile(`(?mi)^\s*([[:alnum:]_-]+\.)*(token|password|clientsecret)\s*=`)

type InstallOptions struct {
	FRPCConfigPath    string
	TokenFilePath     string
	MonitorConfigPath string
	WebhookConfigPath string
	Image             string
	SecretName        string
	ReplaceToken      bool
}

func install(options InstallOptions, stdout, stderr io.Writer) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("install must run as root")
	}
	if err := validateInstallOptions(options); err != nil {
		return err
	}
	for _, command := range []string{"podman", "systemctl"} {
		if _, err := exec.LookPath(command); err != nil {
			return fmt.Errorf("required command %s is unavailable", command)
		}
	}
	if info, err := os.Stat(quadletBinaryPath); err != nil || info.Mode()&0o111 == 0 {
		return fmt.Errorf("required Quadlet generator %s is unavailable", quadletBinaryPath)
	}

	frpcConfig, err := os.ReadFile(options.FRPCConfigPath)
	if err != nil {
		return fmt.Errorf("read frpc config: %w", err)
	}
	if err := validateFRPCConfig(frpcConfig); err != nil {
		return err
	}
	tokenFileData, err := os.ReadFile(options.TokenFilePath)
	if err != nil {
		return fmt.Errorf("read token file: %w", err)
	}
	defer clear(tokenFileData)
	token := bytes.TrimSpace(tokenFileData)
	if len(token) == 0 {
		return fmt.Errorf("token file is empty")
	}

	monitorConfig, webhookCredentials, monitorEnabled, err := prepareMonitorFiles(options)
	if err != nil {
		return err
	}
	if err := installSelf(); err != nil {
		return err
	}
	if err := atomicWrite(frpcConfigPath, append(bytes.TrimRight(frpcConfig, "\r\n"), '\n'), 0o644); err != nil {
		return err
	}
	if err := installPodmanSecret(options.SecretName, token, options.ReplaceToken, stdout, stderr); err != nil {
		return err
	}
	if err := atomicWrite(quadletPath, []byte(renderQuadlet(options.Image, options.SecretName)), 0o644); err != nil {
		return err
	}
	if err := runCommand(nil, io.Discard, stderr, quadletBinaryPath, "-dryrun"); err != nil {
		return fmt.Errorf("validate Quadlet: %w", err)
	}
	if monitorEnabled {
		if err := writeMonitorFiles(monitorConfig, webhookCredentials); err != nil {
			return err
		}
	}
	if err := runCommand(nil, stdout, stderr, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := runCommand(nil, stdout, stderr, "systemctl", "restart", "frpc.service"); err != nil {
		return err
	}
	if monitorEnabled {
		if err := runCommand(nil, stdout, stderr, "systemctl", "enable", "--now", "frp-monitor.timer"); err != nil {
			return err
		}
	}
	fmt.Fprintln(stdout, "frpc installed and started")
	if monitorEnabled {
		fmt.Fprintln(stdout, "independent frp monitor timer installed and started")
	}
	return nil
}

func validateInstallOptions(options InstallOptions) error {
	if options.FRPCConfigPath == "" || options.TokenFilePath == "" {
		return fmt.Errorf("--frpc-config and --token-file are required")
	}
	if (options.MonitorConfigPath == "") != (options.WebhookConfigPath == "") {
		return fmt.Errorf("--monitor-config and --webhook-config must be provided together")
	}
	validReference := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@:+-]*$`)
	if !validReference.MatchString(options.Image) {
		return fmt.Errorf("invalid container image reference")
	}
	validSecretName := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	if !validSecretName.MatchString(options.SecretName) {
		return fmt.Errorf("invalid Podman secret name")
	}
	return nil
}

func validateFRPCConfig(config []byte) error {
	text := string(config)
	if credentialLiteralPattern.Match(config) {
		return fmt.Errorf("frpc config must not contain credential literals")
	}
	required := []string{
		`auth.tokenSource.type = "file"`,
		`auth.tokenSource.file.path = "/run/secrets/frp-auth"`,
	}
	for _, expected := range required {
		if !strings.Contains(text, expected) {
			return fmt.Errorf("frpc config must contain %s", expected)
		}
	}
	return nil
}

func prepareMonitorFiles(options InstallOptions) ([]byte, []byte, bool, error) {
	if options.MonitorConfigPath == "" {
		return nil, nil, false, nil
	}
	config, err := loadMonitorConfig(options.MonitorConfigPath)
	if err != nil {
		return nil, nil, false, err
	}
	for name, path := range map[string]string{
		"SSH host key": config.HostKeyPath,
		"ssh-keyscan":  config.SSHKeyscanPath,
	} {
		if _, err := os.Stat(path); err != nil {
			return nil, nil, false, fmt.Errorf("%s file %s is unavailable: %w", name, path, err)
		}
	}
	credentials, err := loadWebhookCredentials(options.WebhookConfigPath)
	if err != nil {
		return nil, nil, false, err
	}
	config.WebhookCredentialsPath = defaultWebhookCredentialsPath
	config.StatePath = defaultStatePath
	configData, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, nil, false, fmt.Errorf("encode monitor config: %w", err)
	}
	credentialsData, err := json.MarshalIndent(credentials, "", "  ")
	if err != nil {
		return nil, nil, false, fmt.Errorf("encode webhook credentials: %w", err)
	}
	return append(configData, '\n'), append(credentialsData, '\n'), true, nil
}

func installSelf() error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current executable: %w", err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		return fmt.Errorf("read current executable: %w", err)
	}
	return atomicWrite(installedBinaryPath, data, 0o755)
}

func installPodmanSecret(name string, token []byte, replace bool, stdout, stderr io.Writer) error {
	exists, err := podmanSecretExists(name)
	if err != nil {
		return err
	}
	if exists && !replace {
		fmt.Fprintf(stdout, "Podman secret %s already exists; preserving it\n", name)
		return nil
	}
	arguments := []string{"secret", "create"}
	if exists {
		arguments = append(arguments, "--replace")
	}
	arguments = append(arguments, name, "-")
	return runCommand(bytes.NewReader(token), io.Discard, stderr, "podman", arguments...)
}

func podmanSecretExists(name string) (bool, error) {
	command := exec.Command("podman", "secret", "exists", name)
	err := command.Run()
	if err == nil {
		return true, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("inspect Podman secret %s: %w", name, err)
}

func writeMonitorFiles(config, credentials []byte) error {
	if err := os.MkdirAll("/etc/frp-monitor", 0o700); err != nil {
		return fmt.Errorf("create monitor config directory: %w", err)
	}
	if err := os.Chmod("/etc/frp-monitor", 0o700); err != nil {
		return fmt.Errorf("secure monitor config directory: %w", err)
	}
	if err := os.MkdirAll("/var/lib/frp-monitor", 0o700); err != nil {
		return fmt.Errorf("create monitor state directory: %w", err)
	}
	if err := os.Chmod("/var/lib/frp-monitor", 0o700); err != nil {
		return fmt.Errorf("secure monitor state directory: %w", err)
	}
	files := []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{defaultMonitorConfigPath, config, 0o644},
		{defaultWebhookCredentialsPath, credentials, 0o600},
		{monitorServicePath, []byte(renderMonitorService()), 0o644},
		{monitorTimerPath, []byte(renderMonitorTimer()), 0o644},
	}
	for _, file := range files {
		if err := atomicWrite(file.path, file.data, file.mode); err != nil {
			return err
		}
	}
	return nil
}
