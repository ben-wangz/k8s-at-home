package main

import (
	"errors"
	"flag"
)

func runProjects(cli client, raw bool, args []string) error {
	if len(args) == 0 || args[0] == "list" {
		var response listResponse[project]
		if err := cli.get("/projects", &response); err != nil {
			return err
		}
		return printOutput(raw, response.Items)
	}

	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("projects create", flag.ContinueOnError)
		slug := fs.String("slug", "", "slug")
		title := fs.String("title", "", "title")
		description := fs.String("description", "", "description")
		state := fs.String("state", "active", "state")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *slug == "" || *title == "" {
			return errors.New("projects create requires --slug and --title")
		}
		body := map[string]any{"slug": *slug, "title": *title, "description": *description, "state": *state}
		var response project
		if err := cli.postJSON("/projects", body, &response); err != nil {
			return err
		}
		return printOutput(raw, response)
	default:
		return usageError()
	}
}
