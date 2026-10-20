package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/ben-wangz/clash-tests/internal/environment"
	"github.com/ben-wangz/clash-tests/internal/runner"
)

type selections []string

func (values *selections) String() string         { return fmt.Sprint([]string(*values)) }
func (values *selections) Set(value string) error { *values = append(*values, value); return nil }

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := execute(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "clash-tests:", err)
		os.Exit(1)
	}
}

func execute(ctx context.Context, arguments []string) error {
	if len(arguments) == 0 {
		fmt.Println("clash-tests: list | plan | check | run | environment create|inspect|destroy")
		return nil
	}
	operation := arguments[0]
	arguments = arguments[1:]
	if operation == "environment" {
		if len(arguments) == 0 {
			return fmt.Errorf("environment requires create, inspect, or destroy")
		}
		operation = "environment-" + arguments[0]
		arguments = arguments[1:]
	}
	flags := flag.NewFlagSet(operation, flag.ContinueOnError)
	app := flags.String("app-root", os.Getenv("CLASH_TEST_APP_ROOT"), "absolute Clash application directory")
	scenarioID := flags.String("scenario", "feature-minimal", "scenario ID")
	id := flags.String("id", "", "unique run ID")
	reuse := flags.Bool("reuse", false, "explicitly reuse a preserved environment")
	mock := flags.Bool("mock", false, "run browser cases against the local mock controller")
	var selected selections
	flags.Var(&selected, "case", "selected case; repeatable")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if *app == "" || !filepath.IsAbs(*app) {
		return fmt.Errorf("set CLASH_TEST_APP_ROOT or --app-root to the absolute Clash application directory")
	}
	definitions, scenarios, err := runner.Registry(*app)
	if err != nil {
		return err
	}
	scenario, exists := scenarios[*scenarioID]
	if !exists {
		return fmt.Errorf("unknown scenario %s", *scenarioID)
	}
	if *reuse && *id != "" {
		_, record, err := environment.Load(*id)
		if err != nil {
			return err
		}
		scenario = scenarios[record.Scenario]
	}
	switch operation {
	case "list", "plan":
		plan, err := runner.ScenarioPlan(definitions, scenario, selected)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(plan)
	case "check":
		env := environment.New(*app)
		if err := env.Check(ctx, scenario.ID); err != nil {
			return err
		}
		fmt.Printf("Ready: context=%s namespace=%s scenario=%s\n", env.Context, env.Namespace, scenario.ID)
		return nil
	case "run", "environment-create":
		return runner.Run(ctx, *app, definitions, scenario, runner.Options{ID: *id, Scenario: scenario.ID, Selected: selected, Reuse: *reuse, Mock: *mock, CreateOnly: operation == "environment-create"})
	case "environment-inspect", "environment-destroy":
		directory, record, err := environment.Load(*id)
		if err != nil {
			return err
		}
		if operation == "environment-inspect" {
			return json.NewEncoder(os.Stdout).Encode(record)
		}
		unlock, err := environment.LocalLock(*id)
		if err != nil {
			return err
		}
		defer unlock()
		env := environment.Environment{App: *app, Namespace: record.Namespace, Context: record.Context}
		if record.Mode != "integration" {
			return fmt.Errorf("run has no cluster environment")
		}
		work, cancel := context.WithCancelCause(ctx)
		defer cancel(nil)
		release, err := env.Lease(work, *id, cancel)
		if err != nil {
			return err
		}
		defer release()
		if err := env.Destroy(work, directory, record); err != nil {
			return err
		}
		fmt.Printf("Destroyed environment %s\n", record.ID)
		return nil
	default:
		return fmt.Errorf("unknown command %s", operation)
	}
}
