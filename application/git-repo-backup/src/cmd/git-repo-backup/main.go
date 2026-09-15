// Command git-repo-backup performs one Git repository backup run.
package main

import (
	"os"

	"git-repo-backup/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
