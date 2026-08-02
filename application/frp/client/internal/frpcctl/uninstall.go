package frpcctl

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

func uninstall(purge bool, secretName string, stdout, stderr io.Writer) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("uninstall must run as root")
	}
	timerCommand := exec.Command("systemctl", "disable", "--now", "frp-monitor.timer")
	timerCommand.Stdout = stdout
	timerCommand.Stderr = stderr
	if err := timerCommand.Run(); err != nil {
		fmt.Fprintf(stderr, "warning: unable to disable frp-monitor.timer: %v\n", err)
	}
	stopCommand := exec.Command("systemctl", "stop", "frpc.service")
	stopCommand.Stdout = stdout
	stopCommand.Stderr = stderr
	if err := stopCommand.Run(); err != nil {
		fmt.Fprintf(stderr, "warning: unable to stop frpc.service: %v\n", err)
	}
	for _, path := range []string{quadletPath, monitorServicePath, monitorTimerPath} {
		if err := removeIfPresent(path); err != nil {
			return err
		}
	}
	if purge {
		if err := purgeClientData(secretName, stdout, stderr); err != nil {
			return err
		}
	}
	if err := runCommand(nil, stdout, stderr, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := removeIfPresent(installedBinaryPath); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "frpc services uninstalled")
	return nil
}

func purgeClientData(secretName string, stdout, stderr io.Writer) error {
	for _, path := range []string{frpcConfigPath, defaultMonitorConfigPath, defaultWebhookCredentialsPath, defaultStatePath} {
		if err := removeIfPresent(path); err != nil {
			return err
		}
	}
	exists, err := podmanSecretExists(secretName)
	if err != nil {
		return err
	}
	if exists {
		if err := runCommand(nil, stdout, stderr, "podman", "secret", "rm", secretName); err != nil {
			return err
		}
	}
	return nil
}
