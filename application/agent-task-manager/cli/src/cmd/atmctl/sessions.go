package main

import (
	"errors"
	"flag"
)

func runSessions(cli client, raw bool, args []string) error {
	if len(args) == 0 {
		return usageError()
	}
	switch args[0] {
	case "list":
		var response listResponse[session]
		if err := cli.get("/sessions", &response); err != nil {
			return err
		}
		return printOutput(raw, response.Items)
	case "get":
		if len(args) < 2 {
			return errors.New("sessions get requires snapshot id")
		}
		var response session
		if err := cli.get("/sessions/"+args[1], &response); err != nil {
			return err
		}
		return printOutput(raw, response)
	case "register":
		fs := flag.NewFlagSet("sessions register", flag.ContinueOnError)
		title := fs.String("title", "", "title")
		description := fs.String("description", "", "description")
		artifactName := fs.String("artifact-name", "", "artifact name")
		artifactPath := fs.String("artifact-path", "", "artifact path")
		snapshotID := fs.String("snapshot-id", "", "snapshot id")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *title == "" || *artifactName == "" || *artifactPath == "" {
			return errors.New("sessions register requires --title, --artifact-name, and --artifact-path")
		}
		body := map[string]any{"snapshot_id": *snapshotID, "type": "opencode", "title": *title, "description": *description, "artifact_name": *artifactName, "artifact_path": *artifactPath}
		var response session
		if err := cli.postJSON("/sessions", body, &response); err != nil {
			return err
		}
		return printOutput(raw, response)
	case "upload":
		fs := flag.NewFlagSet("sessions upload", flag.ContinueOnError)
		title := fs.String("title", "", "title")
		description := fs.String("description", "", "description")
		filePath := fs.String("file", "", "artifact file path")
		snapshotID := fs.String("snapshot-id", "", "snapshot id")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *title == "" || *filePath == "" {
			return errors.New("sessions upload requires --title and --file")
		}
		uploaded, err := cli.uploadFile("/sessions/uploads", *filePath)
		if err != nil {
			return err
		}
		body := map[string]any{"snapshot_id": *snapshotID, "type": "opencode", "title": *title, "description": *description, "artifact_name": uploaded.ArtifactName, "artifact_path": uploaded.ArtifactPath}
		var response session
		if err := cli.postJSON("/sessions", body, &response); err != nil {
			return err
		}
		return printOutput(raw, response)
	case "download":
		if len(args) < 3 {
			return errors.New("sessions download requires snapshot id and output path")
		}
		return cli.downloadFile("/sessions/"+args[1]+"/download", args[2])
	default:
		return usageError()
	}
}
