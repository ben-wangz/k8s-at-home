package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"git-repo-backup/internal/observability"
	"git-repo-backup/internal/runner"
)

// runCommand executes one full backup run. Signals trigger a bounded
// cleanup: the cancellation propagates into Git subprocesses and S3 aborts,
// then the process exits with the signal-specific code.
func runCommand(args []string) int {
	path, code, ok := parseConfigFlag("run", args)
	if !ok {
		return code
	}
	cfg, err := loadConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-repo-backup: %s\n", safeMessage(err))
		return exitInput
	}
	logger, err := observability.NewLogger(cfg.Log.Level)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-repo-backup: %s\n", safeMessage(err))
		return exitInput
	}
	repos, err := loadRepositories(cfg)
	if err != nil {
		logger.Error("repository list invalid", "errorCode", observability.CodeOf(err))
		return exitInput
	}

	// Restrictive umask for every private and temporary file this process
	// creates, inherited by Git and SSH subprocesses.
	syscall.Umask(0o077)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var received atomic.Value // stores os.Signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		if sig, ok := <-sigCh; ok {
			received.Store(sig)
			cancel()
		}
	}()

	podUID := os.Getenv("POD_UID")
	if podUID == "" {
		podUID = runner.NewOwnerUUID()
	}
	result, runErr := runner.Run(ctx, runner.Options{
		Config: cfg,
		Logger: logger,
		Repos:  repos,
		PodUID: podUID,
		Now:    runner.DefaultClock,
	})
	if runErr == nil {
		return exitOK
	}
	logger.Error("run failed", "event", "run_failed",
		"errorCode", observability.CodeOf(runErr),
		"published", result.Published,
		"message", safeMessage(runErr))
	if sig, ok := received.Load().(os.Signal); ok {
		if sig == os.Interrupt {
			return exitSIGINT
		}
		return exitSIGTERM
	}
	return exitForError(runErr)
}
