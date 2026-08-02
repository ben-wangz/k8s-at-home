package frpcctl

import (
	"flag"
	"fmt"
	"io"
)

func Run(arguments []string, stdout, stderr io.Writer) int {
	if len(arguments) == 0 {
		printUsage(stderr)
		return 2
	}
	var err error
	switch arguments[0] {
	case "install":
		err = runInstall(arguments[1:], stdout, stderr)
	case "monitor":
		err = runMonitor(arguments[1:], stdout, stderr)
	case "webhook-test":
		err = runWebhookTest(arguments[1:], stdout, stderr)
	case "uninstall":
		err = runUninstall(arguments[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", arguments[0])
		printUsage(stderr)
		return 2
	}
	if err == nil {
		return 0
	}
	if probeFailure, ok := err.(probeFailureError); ok {
		fmt.Fprintln(stderr, probeFailure.Error())
		return 1
	}
	fmt.Fprintf(stderr, "error: %v\n", err)
	return 2
}

func runInstall(arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	flags.SetOutput(stderr)
	options := InstallOptions{}
	flags.StringVar(&options.FRPCConfigPath, "frpc-config", "", "path to the non-sensitive frpc TOML file")
	flags.StringVar(&options.TokenFilePath, "token-file", "", "path to the frp authentication token")
	flags.StringVar(&options.MonitorConfigPath, "monitor-config", "", "path to the non-sensitive monitor JSON file")
	flags.StringVar(&options.WebhookConfigPath, "webhook-config", "", "path to the sensitive webhook credentials JSON file")
	flags.StringVar(&options.Image, "image", "docker.io/fatedier/frpc:v0.70.1", "frpc container image")
	flags.StringVar(&options.SecretName, "secret-name", "frp-auth", "Podman Secret name")
	flags.BoolVar(&options.ReplaceToken, "replace-token", false, "replace an existing Podman Secret from --token-file")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("install does not accept positional arguments")
	}
	return install(options, stdout, stderr)
}

func runMonitor(arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("monitor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := defaultMonitorConfigPath
	flags.StringVar(&configPath, "config", configPath, "path to the monitor JSON file")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("monitor does not accept positional arguments")
	}
	config, err := loadMonitorConfig(configPath)
	if err != nil {
		return err
	}
	return monitorOnce(config, stdout)
}

func runWebhookTest(arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("webhook-test", flag.ContinueOnError)
	flags.SetOutput(stderr)
	credentialsPath := defaultWebhookCredentialsPath
	message := "frpcctl webhook test"
	flags.StringVar(&credentialsPath, "credentials", credentialsPath, "path to the webhook credentials JSON file")
	flags.StringVar(&message, "message", message, "test message")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("webhook-test does not accept positional arguments")
	}
	credentials, err := loadWebhookCredentials(credentialsPath)
	if err != nil {
		return err
	}
	if err := sendDingTalk(credentials, message, 10); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "webhook test sent")
	return nil
}

func runUninstall(arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	flags.SetOutput(stderr)
	purge := false
	secretName := "frp-auth"
	flags.BoolVar(&purge, "purge", false, "also remove configuration, monitor state, and the Podman Secret")
	flags.StringVar(&secretName, "secret-name", secretName, "Podman Secret name to remove with --purge")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("uninstall does not accept positional arguments")
	}
	return uninstall(purge, secretName, stdout, stderr)
}

func printUsage(output io.Writer) {
	fmt.Fprintln(output, `Usage: frpcctl <command> [options]

Commands:
  install       Install frpc as a rootful Podman Quadlet
  monitor       Run one independent end-to-end tunnel probe
  webhook-test  Send a standalone DingTalk webhook test
  uninstall     Remove installed services; preserve data unless --purge is used`)
}
