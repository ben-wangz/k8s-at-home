package main

import (
	"flag"
	"strings"
)

func runActivities(cli client, raw bool, args []string) error {
	if len(args) == 0 || args[0] == "list" {
		fs := flag.NewFlagSet("activities list", flag.ContinueOnError)
		projectID := fs.String("project", "", "project id")
		taskID := fs.String("task", "", "task id")
		label := fs.String("label", "", "label")
		sort := fs.String("sort", "desc", "sort asc|desc")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		params := make([]string, 0, 4)
		addQuery(&params, "project", *projectID)
		addQuery(&params, "task", *taskID)
		addQuery(&params, "label", *label)
		addQuery(&params, "sort", *sort)
		path := "/activities"
		if len(params) > 0 {
			path += "?" + strings.Join(params, "&")
		}
		var response listResponse[activity]
		if err := cli.get(path, &response); err != nil {
			return err
		}
		return printOutput(raw, response.Items)
	}
	return usageError()
}
