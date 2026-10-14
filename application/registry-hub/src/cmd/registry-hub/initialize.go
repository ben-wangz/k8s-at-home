package main

import (
	"context"
	"errors"
	"os"
	"strconv"
	"time"

	"registry-hub/internal/initialize"
)

func initializeInternal() error {
	options, err := initialize.OptionsFromEnvironment()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), initialize.JobTimeout())
	defer cancel()
	return initialize.Run(ctx, options)
}

func waitForInitialization() error {
	recordFile := os.Getenv("REGISTRY_HUB_INIT_RECORD_FILE")
	scope := os.Getenv("REGISTRY_HUB_INIT_SCOPE")
	protocol := os.Getenv("REGISTRY_HUB_INIT_WAIT_PROTOCOL")
	if protocol == "" {
		protocol = "v1"
	}
	seconds, err := strconv.Atoi(os.Getenv("REGISTRY_HUB_INIT_WAIT_TIMEOUT_SECONDS"))
	if err != nil || seconds < 1 {
		return errors.New("initialization wait timeout must be a positive integer")
	}
	return initialize.Wait(context.Background(), recordFile, scope, protocol, time.Duration(seconds)*time.Second)
}
