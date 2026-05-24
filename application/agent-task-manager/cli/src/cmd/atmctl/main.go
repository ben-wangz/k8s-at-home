package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return usageError()
	}
	cfg := loadConfig()
	cli := client{baseURL: strings.TrimRight(cfg.BaseURL, "/"), apiKey: cfg.APIKey, http: http.DefaultClient}
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--raw" {
		cfg.Raw = true
		args = args[1:]
	}
	if len(args) == 0 {
		return usageError()
	}

	switch args[0] {
	case "projects":
		return runProjects(cli, cfg.Raw, args[1:])
	case "tasks":
		return runTasks(cli, cfg.Raw, args[1:])
	case "sessions":
		return runSessions(cli, cfg.Raw, args[1:])
	case "activities":
		return runActivities(cli, cfg.Raw, args[1:])
	default:
		return usageError()
	}
}
