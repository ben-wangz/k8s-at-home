package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/config"
	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/httpapi"
	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/service"
	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/store/bunstore"
)

func main() {
	cfg := config.Load()
	store, err := bunstore.Open(cfg.Database)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           httpapi.New(service.New(store), cfg.ArtifactsDir, cfg.AuthMode).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Printf("agent-task-manager API listening on %s using %s", cfg.ListenAddr, cfg.Database.Driver)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
