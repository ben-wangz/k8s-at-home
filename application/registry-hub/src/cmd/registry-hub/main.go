package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"registry-hub/internal/app"
	"registry-hub/internal/config"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "initialize":
			if err := initializeInternal(); err != nil {
				slog.Error("initialization failed", "error", err)
				os.Exit(1)
			}
			return
		case "initialize-internal":
			if err := initializeInternal(); err != nil {
				slog.Error("internal initialization failed", "error", err)
				os.Exit(1)
			}
			return
		case "wait-initialization":
			if err := waitForInitialization(); err != nil {
				slog.Error("initialization wait failed", "error", err)
				os.Exit(1)
			}
			return
		case "wait-internal":
			if err := waitForInitialization(); err != nil {
				slog.Error("internal initialization wait failed", "error", err)
				os.Exit(1)
			}
			return
		}
	}

	cfg, err := config.Load()
	if err != nil {
		logger.Error("configuration invalid", "error", err)
		os.Exit(2)
	}
	if cfg.ClientAuth.Empty() {
		logger.Warn("client authentication is disabled; registry operations are allowed to trusted network clients")
	}

	service, err := app.New(cfg, logger)
	if err != nil {
		logger.Error("service initialization failed", "error", err)
		os.Exit(1)
	}
	defer service.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 2)
	clientServer := &http.Server{Addr: cfg.ClientAddr, Handler: service.ClientHandler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	adminServer := &http.Server{Addr: cfg.AdminAddr, Handler: service.AdminHandler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	go func() { errCh <- clientServer.ListenAndServe() }()
	go func() { errCh <- adminServer.ListenAndServe() }()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("listener stopped", "error", err)
		}
	}

	service.BeginShutdown()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = clientServer.Shutdown(shutdownCtx)
	_ = adminServer.Shutdown(shutdownCtx)
}
