package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases"
	"github.com/ben-wangz/registry-hub-tests/internal/runner"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := execute(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "registry-hub-tests:", err)
		os.Exit(1)
	}
}

func execute(ctx context.Context, arguments []string) error {
	root := os.Getenv("REGISTRY_HUB_TEST_REPO_ROOT")
	if len(arguments) >= 2 && arguments[0] == "--repo-root" {
		root = arguments[1]
		arguments = arguments[2:]
	}
	if root == "" {
		return fmt.Errorf("set REGISTRY_HUB_TEST_REPO_ROOT or pass --repo-root")
	}
	if err := os.Setenv("REGISTRY_HUB_TEST_REPO_ROOT", root); err != nil {
		return err
	}
	registered, err := cases.Registry()
	if err != nil {
		return err
	}
	runtime, err := runner.New(root, registered)
	if err != nil {
		return err
	}
	if len(arguments) == 0 {
		printUsage()
		return nil
	}
	switch arguments[0] {
	case "list":
		return listScenarioCases(runtime, arguments[1:])
	case "plan":
		return planCases(runtime, arguments[1:])
	case "check":
		return checkEnvironment(ctx, runtime, arguments[1:])
	case "run":
		return runSuite(ctx, runtime, arguments[1:])
	case "environment":
		return environmentCommand(ctx, runtime, arguments[1:])
	case "case":
		return runCase(ctx, runtime, arguments[1:])
	case "help", "--help", "-h":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

func listCases(registered []testkit.Case) error {
	sort.Slice(registered, func(i, j int) bool {
		return registered[i].Definition.ID < registered[j].Definition.ID
	})
	for _, item := range registered {
		dependencies := strings.Join(item.Definition.DependsOn, ", ")
		fmt.Printf("%-24s %-8s %s\n", item.Definition.ID, duration(item.Definition.TimeoutSeconds), item.Definition.Description)
		if dependencies != "" {
			fmt.Printf("  depends on: %s\n", dependencies)
		}
	}
	return nil
}

func planCases(runtime *runner.Runtime, arguments []string) error {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	scenario := flags.String("scenario", "feature-minimal", "test scenario ID")
	var selected stringList
	flags.Var(&selected, "case", "case ID to include; repeatable")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected plan arguments: %s", strings.Join(flags.Args(), " "))
	}
	plan, err := runtime.PlanScenario(*scenario, selected)
	if err != nil {
		return err
	}
	printPlan(plan)
	return nil
}

func checkEnvironment(ctx context.Context, runtime *runner.Runtime, arguments []string) error {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	scenario := flags.String("scenario", "feature-minimal", "test scenario ID")
	noBuild := flags.Bool("no-build", false, "skip Podman requirement")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if _, err := runtime.CasesForScenario(*scenario); err != nil {
		return err
	}
	result, err := runtime.Check(ctx, !*noBuild)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

func runSuite(ctx context.Context, runtime *runner.Runtime, arguments []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	id := flags.String("id", "", "unique test run ID")
	scenario := flags.String("scenario", "feature-minimal", "test scenario ID")
	reuse := flags.Bool("reuse", false, "resume a preserved run")
	image := flags.String("image", "", "prebuilt repository@sha256 digest")
	planOnly := flags.Bool("plan", false, "print the execution plan without cluster actions")
	listOnly := flags.Bool("list", false, "list registered cases without cluster actions")
	var selected stringList
	flags.Var(&selected, "case", "case ID to run; repeatable")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected run arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *listOnly {
		return listScenarioCases(runtime, []string{"--scenario", *scenario})
	}
	if *planOnly {
		plan, err := runtime.PlanScenario(*scenario, selected)
		if err != nil {
			return err
		}
		printPlan(plan)
		return nil
	}
	return runtime.Run(ctx, runner.RunOptions{ID: *id, Scenario: *scenario, Selected: selected, Reuse: *reuse, Image: *image})
}

func environmentCommand(ctx context.Context, runtime *runner.Runtime, arguments []string) error {
	if len(arguments) == 0 {
		return fmt.Errorf("environment requires check, create, inspect, or destroy")
	}
	switch arguments[0] {
	case "check":
		return checkEnvironment(ctx, runtime, arguments[1:])
	case "create":
		flags := flag.NewFlagSet("environment create", flag.ContinueOnError)
		id := flags.String("id", "", "test run ID")
		scenario := flags.String("scenario", "feature-minimal", "test scenario ID")
		reuse := flags.Bool("reuse", false, "resume a preserved run")
		image := flags.String("image", "", "prebuilt repository@sha256 digest")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if *id == "" {
			return fmt.Errorf("environment create requires --id")
		}
		return runtime.Run(ctx, runner.RunOptions{ID: *id, Scenario: *scenario, Reuse: *reuse, Image: *image, CreateOnly: true})
	case "inspect", "destroy":
		flags := flag.NewFlagSet("environment "+arguments[0], flag.ContinueOnError)
		id := flags.String("id", "", "test run ID")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if *id == "" {
			return fmt.Errorf("environment %s requires --id", arguments[0])
		}
		if arguments[0] == "inspect" {
			return runtime.Inspect(ctx, *id)
		}
		return runtime.Destroy(ctx, *id)
	default:
		return fmt.Errorf("unknown environment command %q", arguments[0])
	}
}

func runCase(ctx context.Context, runtime *runner.Runtime, arguments []string) error {
	flags := flag.NewFlagSet("case", flag.ContinueOnError)
	id := flags.String("id", "", "registered case ID")
	contextFile := flags.String("context-file", "", "protected run context file")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *id == "" || *contextFile == "" || flags.NArg() != 0 {
		return fmt.Errorf("case requires --id and --context-file")
	}
	return runner.ExecuteCase(ctx, runtime.Cases, *id, *contextFile)
}

func caseValues(indexed map[string]testkit.Case) []testkit.Case {
	result := make([]testkit.Case, 0, len(indexed))
	for _, item := range indexed {
		result = append(result, item)
	}
	return result
}

func listScenarioCases(runtime *runner.Runtime, arguments []string) error {
	flags := flag.NewFlagSet("list", flag.ContinueOnError)
	scenario := flags.String("scenario", "feature-minimal", "test scenario ID")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected list arguments: %s", strings.Join(flags.Args(), " "))
	}
	registered, err := runtime.CasesForScenario(*scenario)
	if err != nil {
		return err
	}
	return listCases(registered)
}

func printPlan(plan runner.Plan) {
	fmt.Printf("Selected cases: %s\n", strings.Join(plan.Explicit, ", "))
	if len(plan.Added) > 0 {
		fmt.Printf("Added dependencies: %s\n", strings.Join(plan.Added, ", "))
	}
	for _, edge := range plan.Edges {
		fmt.Printf("  %s -> %s\n", edge.Dependency, edge.Consumer)
	}
	fmt.Printf("Execution order: %s\n", strings.Join(plan.Order, " -> "))
}
