package main

import (
	"errors"
	"flag"
	"strings"
)

func runTasks(cli client, raw bool, args []string) error {
	if len(args) == 0 {
		return usageError()
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("tasks list", flag.ContinueOnError)
		projectID := fs.String("project", "", "project id")
		query := fs.String("q", "", "query")
		status := fs.String("status", "", "status")
		priority := fs.String("priority", "", "priority")
		label := fs.String("label", "", "label")
		assignee := fs.String("assignee", "", "assignee id")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		path := "/tasks"
		params := make([]string, 0, 5)
		addQuery(&params, "q", *query)
		addQuery(&params, "status", *status)
		addQuery(&params, "priority", *priority)
		addQuery(&params, "label", *label)
		addQuery(&params, "assignee_id", *assignee)
		if *projectID != "" {
			path = "/projects/" + *projectID + "/tasks"
		}
		if len(params) > 0 {
			path += "?" + strings.Join(params, "&")
		}
		var response listResponse[task]
		if err := cli.get(path, &response); err != nil {
			return err
		}
		return printOutput(raw, response.Items)
	case "get":
		getArgs := normalizeCommandArgs(args[1:])
		if len(getArgs) == 0 {
			return errors.New("tasks get requires task id")
		}
		var response any
		if err := cli.get("/tasks/"+getArgs[0], &response); err != nil {
			return err
		}
		return printOutput(raw, response)
	case "create":
		fs := flag.NewFlagSet("tasks create", flag.ContinueOnError)
		projectID := fs.String("project", "", "project id")
		title := fs.String("title", "", "title")
		description := fs.String("description", "", "description")
		priority := fs.String("priority", "P1", "priority")
		labels := fs.String("labels", "", "comma-separated labels")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *projectID == "" || *title == "" {
			return errors.New("tasks create requires --project <project-id> and --title")
		}
		body := map[string]any{"title": *title, "description": *description, "status": "backlog", "priority": *priority, "labels": splitCSV(*labels)}
		var response task
		if err := cli.postJSON("/projects/"+*projectID+"/tasks", body, &response); err != nil {
			return err
		}
		return printOutput(raw, response)
	case "update":
		fs := flag.NewFlagSet("tasks update", flag.ContinueOnError)
		title := fs.String("title", "", "title")
		description := fs.String("description", "", "description")
		status := fs.String("status", "", "status")
		priority := fs.String("priority", "", "priority")
		assignee := fs.String("assignee", "", "assignee id or unassigned")
		labels := fs.String("labels", "", "comma-separated labels")
		updateArgs := normalizeCommandArgs(args[1:])
		if err := fs.Parse(updateArgs[1:]); err != nil {
			return err
		}
		if len(updateArgs) == 0 {
			return errors.New("tasks update requires task id")
		}
		body := map[string]any{}
		if *title != "" {
			body["title"] = *title
		}
		if *description != "" {
			body["description"] = *description
		}
		if *status != "" {
			body["status"] = *status
		}
		if *priority != "" {
			body["priority"] = *priority
		}
		if *assignee != "" {
			if *assignee == "unassigned" {
				body["assignee_id"] = nil
			} else {
				body["assignee_id"] = *assignee
			}
		}
		if *labels != "" {
			body["labels"] = splitCSV(*labels)
		}
		var response task
		if err := cli.patchJSON("/tasks/"+updateArgs[0], body, &response); err != nil {
			return err
		}
		return printOutput(raw, response)
	case "comment":
		fs := flag.NewFlagSet("tasks comment", flag.ContinueOnError)
		body := fs.String("body", "", "comment body")
		authorID := fs.String("author", "", "author id")
		commentArgs := normalizeCommandArgs(args[1:])
		if err := fs.Parse(commentArgs[1:]); err != nil {
			return err
		}
		if len(commentArgs) == 0 || *body == "" {
			return errors.New("tasks comment requires task id and --body")
		}
		payload := map[string]any{"body": *body}
		if *authorID != "" {
			payload["author_id"] = *authorID
		}
		var response any
		if err := cli.postJSON("/tasks/"+commentArgs[0]+"/comments", payload, &response); err != nil {
			return err
		}
		return printOutput(raw, response)
	case "subtasks":
		fs := flag.NewFlagSet("tasks subtasks", flag.ContinueOnError)
		title := fs.String("title", "", "subtask title")
		description := fs.String("description", "", "description")
		status := fs.String("status", "backlog", "status")
		priority := fs.String("priority", "P1", "priority")
		labels := fs.String("labels", "", "comma-separated labels")
		subtaskArgs := normalizeCommandArgs(args[1:])
		if err := fs.Parse(subtaskArgs[1:]); err != nil {
			return err
		}
		if len(subtaskArgs) == 0 || *title == "" {
			return errors.New("tasks subtasks requires parent task id and --title")
		}
		payload := map[string]any{"tasks": []map[string]any{{"title": *title, "description": *description, "status": *status, "priority": *priority, "labels": splitCSV(*labels)}}}
		var response listResponse[task]
		if err := cli.postJSON("/tasks/"+subtaskArgs[0]+"/subtasks", payload, &response); err != nil {
			return err
		}
		return printOutput(raw, response.Items)
	case "reparent":
		fs := flag.NewFlagSet("tasks reparent", flag.ContinueOnError)
		parentID := fs.String("parent", "", "new parent task id")
		reparentArgs := normalizeCommandArgs(args[1:])
		if err := fs.Parse(reparentArgs[1:]); err != nil {
			return err
		}
		if len(reparentArgs) == 0 {
			return errors.New("tasks reparent requires task id")
		}
		payload := map[string]any{"parent_task_id": nil}
		if *parentID != "" {
			payload["parent_task_id"] = *parentID
		}
		var response task
		if err := cli.postJSON("/tasks/"+reparentArgs[0]+"/reparent", payload, &response); err != nil {
			return err
		}
		return printOutput(raw, response)
	default:
		return usageError()
	}
}
