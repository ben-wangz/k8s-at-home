package main

import (
	"fmt"
	"strings"
)

type stringList []string

func (values *stringList) String() string {
	return strings.Join(*values, ",")
}

func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func printUsage() {
	fmt.Println("Usage: registry-hub-tests <list|plan|check|run|environment>")
	fmt.Println("Run 'registry-hub-tests run --help' for run options.")
}
