package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item != "" {
			items = append(items, item)
		}
	}
	return items
}

func addQuery(params *[]string, key, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	*params = append(*params, key+"="+value)
}

func normalizeCommandArgs(args []string) []string {
	if len(args) == 0 {
		return args
	}
	if strings.HasPrefix(args[0], "-") {
		return args
	}
	return append([]string{args[0]}, args[1:]...)
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func printOutput(raw bool, value any) error {
	if !raw {
		return printJSON(value)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, string(data))
	return err
}

func usageError() error {
	return errors.New("usage: atmctl <projects|tasks|sessions|activities> ...")
}
