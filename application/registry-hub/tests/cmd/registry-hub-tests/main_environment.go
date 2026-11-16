package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/runner"
)

func pruneEnvironments(ctx context.Context, runtime *runner.Runtime, arguments []string) error {
	flags := flag.NewFlagSet("environment prune", flag.ContinueOnError)
	olderThan := flags.Duration("older-than", 24*time.Hour, "minimum inactivity before destruction")
	dryRun := flags.Bool("dry-run", false, "list expired environments without deleting resources")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected prune arguments: %s", strings.Join(flags.Args(), " "))
	}
	return runtime.Prune(ctx, *olderThan, *dryRun, "")
}
