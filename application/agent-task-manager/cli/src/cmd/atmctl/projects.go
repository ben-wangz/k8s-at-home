package main

func runProjects(cli client, raw bool, args []string) error {
	if len(args) == 0 || args[0] == "list" {
		var response listResponse[project]
		if err := cli.get("/projects", &response); err != nil {
			return err
		}
		return printOutput(raw, response.Items)
	}
	return usageError()
}
