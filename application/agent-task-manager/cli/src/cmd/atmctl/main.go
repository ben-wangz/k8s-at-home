package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type config struct {
	BaseURL string
	APIKey  string
	Raw     bool
}

type client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

type apiError struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

type listResponse[T any] struct {
	Items []T `json:"items"`
}

type uploadResponse struct {
	ArtifactName string `json:"artifact_name"`
	ArtifactPath string `json:"artifact_path"`
}

type project struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Description string `json:"description"`
	State       string `json:"state"`
	Open        int    `json:"open"`
	InProgress  int    `json:"in_progress"`
	InReview    int    `json:"in_review"`
}

type task struct {
	ID           string   `json:"id"`
	ProjectID    string   `json:"project_id"`
	ParentTaskID *string  `json:"parent_task_id,omitempty"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Status       string   `json:"status"`
	Priority     string   `json:"priority"`
	AssigneeID   *string  `json:"assignee_id,omitempty"`
	Labels       []string `json:"labels"`
}

type session struct {
	SnapshotID   string `json:"snapshot_id"`
	Type         string `json:"type"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	ArtifactName string `json:"artifact_name"`
	ArtifactPath string `json:"artifact_path"`
}

type activity struct {
	ID        string  `json:"id"`
	ProjectID *string `json:"project_id,omitempty"`
	TaskID    *string `json:"task_id,omitempty"`
	Label     string  `json:"label"`
	Kind      string  `json:"kind"`
	Message   string  `json:"message"`
}

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return usageError()
	}
	cfg := loadConfig()
	cli := client{baseURL: strings.TrimRight(cfg.BaseURL, "/"), apiKey: cfg.APIKey, http: http.DefaultClient}
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--raw" {
		cfg.Raw = true
		args = args[1:]
	}
	if len(args) == 0 {
		return usageError()
	}

	switch args[0] {
	case "projects":
		return runProjects(cli, cfg.Raw, args[1:])
	case "tasks":
		return runTasks(cli, cfg.Raw, args[1:])
	case "sessions":
		return runSessions(cli, cfg.Raw, args[1:])
	case "activities":
		return runActivities(cli, cfg.Raw, args[1:])
	default:
		return usageError()
	}
}

func loadConfig() config {
	baseURL := strings.TrimSpace(os.Getenv("ATM_API_URL"))
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8080/api/v1"
	}
	apiKey := strings.TrimSpace(os.Getenv("ATM_API_KEY"))
	raw := strings.EqualFold(strings.TrimSpace(os.Getenv("ATM_OUTPUT")), "raw")
	return config{BaseURL: baseURL, APIKey: apiKey, Raw: raw}
}

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
		if len(args) < 2 {
			return errors.New("tasks get requires task id")
		}
		var response any
		if err := cli.get("/tasks/"+args[1], &response); err != nil {
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
		body := map[string]any{"title": *title, "description": *description, "status": "todo", "priority": *priority, "labels": splitCSV(*labels)}
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
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() < 1 {
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
		if err := cli.patchJSON("/tasks/"+fs.Arg(0), body, &response); err != nil {
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
		status := fs.String("status", "todo", "status")
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

func (c client) get(path string, out any) error {
	return c.doJSON(http.MethodGet, path, nil, out)
}

func (c client) postJSON(path string, body any, out any) error {
	return c.doJSON(http.MethodPost, path, body, out)
}

func (c client) patchJSON(path string, body any, out any) error {
	return c.doJSON(http.MethodPatch, path, body, out)
}

func (c client) doJSON(method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return decodeAPIError(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
	}

func (c client) uploadFile(path, filePath string) (uploadResponse, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return uploadResponse{}, err
	}
	defer file.Close()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return uploadResponse{}, err
	}
	if _, err := io.Copy(part, file); err != nil {
		return uploadResponse{}, err
	}
	if err := writer.Close(); err != nil {
		return uploadResponse{}, err
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, &body)
	if err != nil {
		return uploadResponse{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return uploadResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return uploadResponse{}, decodeAPIError(resp)
	}
	var out uploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return uploadResponse{}, err
	}
	return out, nil
}

func (c client) downloadFile(path, outputPath string) error {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return decodeAPIError(resp)
	}
	out, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

func decodeAPIError(resp *http.Response) error {
	var payload apiError
	if err := json.NewDecoder(resp.Body).Decode(&payload); err == nil && payload.Error != "" {
		if payload.Code != "" {
			return fmt.Errorf("%s: %s", payload.Code, payload.Error)
		}
		return errors.New(payload.Error)
	}
	return fmt.Errorf("request failed: %s", resp.Status)
}

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
