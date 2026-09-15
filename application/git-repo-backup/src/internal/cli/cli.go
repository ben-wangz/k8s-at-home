// Package cli implements the subcommands: run, validate, version, and
// prepare. Exit codes: 0 success, 2 configuration/input errors, 1 runtime,
// publication, or retention errors, 130 SIGINT, 143 SIGTERM.
package cli

import (
	"errors"
	"fmt"
	"os"

	"git-repo-backup/internal/observability"
)

const usage = `usage: git-repo-backup <command> [flags]

commands:
  run       execute one backup run
  validate  decode and validate configuration and local reference files
  version   print the build version
  prepare   initialize mounted volumes and the SSH key (initContainer)
`

// Exit codes.
const (
	exitOK      = 0
	exitFailure = 1
	exitInput   = 2
	exitSIGINT  = 130
	exitSIGTERM = 143
)

// Main dispatches one subcommand and returns the process exit code.
func Main(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return exitInput
	}
	switch args[0] {
	case "run":
		return runCommand(args[1:])
	case "validate":
		return validateCommand(args[1:])
	case "version":
		return versionCommand(args[1:])
	case "prepare":
		return prepareCommand(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "git-repo-backup: unknown command %q\n", args[0])
		fmt.Fprint(os.Stderr, usage)
		return exitInput
	}
}

// exitForError maps an error to a process exit code.
func exitForError(err error) int {
	switch observability.CodeOf(err) {
	case observability.CodeConfigInvalid, observability.CodeInputInvalid, observability.CodeURLInvalid:
		return exitInput
	}
	return exitFailure
}

// safeMessage renders an error for terminal output without echoing wrapped
// causes, which may carry remote-controlled stderr or config fragments.
func safeMessage(err error) string {
	var se *observability.SafeError
	if errors.As(err, &se) {
		return se.Code + ": " + se.Msg
	}
	return observability.CodeOf(err) + ": run failed"
}
