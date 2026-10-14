package testkit

import (
	"context"
	"encoding/json"
	"time"
)

type Definition struct {
	ID             string            `json:"id"`
	Description    string            `json:"description"`
	DependsOn      []string          `json:"dependsOn"`
	TimeoutSeconds int               `json:"timeoutSeconds"`
	Preconditions  []string          `json:"preconditions"`
	Assertions     []string          `json:"assertions"`
	Checks         []CheckDefinition `json:"checks,omitempty"`
	Outputs        []string          `json:"outputs"`
	Requirements   []string          `json:"requirements"`
}

type CheckDefinition struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

type CheckResult struct {
	ID       string   `json:"id"`
	Status   string   `json:"status"`
	Evidence []string `json:"evidence,omitempty"`
}

type Case struct {
	Definition Definition
	Raw        json.RawMessage
	Run        func(context.Context, CaseContext) error
}

type CaseContext struct {
	RunID             string                    `json:"runId"`
	ScenarioID        string                    `json:"scenarioId"`
	CaseID            string                    `json:"caseId"`
	Namespace         string                    `json:"namespace"`
	Release           string                    `json:"release"`
	Fullname          string                    `json:"fullname"`
	KubeContext       string                    `json:"kubeContext"`
	ClientURL         string                    `json:"clientUrl"`
	AdminURL          string                    `json:"adminUrl"`
	ChartDir          string                    `json:"chartDir"`
	BaseValues        string                    `json:"baseValues"`
	ValuesFile        string                    `json:"valuesFile"`
	ImageReference    string                    `json:"imageReference"`
	ImageDigest       string                    `json:"imageDigest"`
	StateDir          string                    `json:"stateDir"`
	OutputDir         string                    `json:"outputDir"`
	DependencyResults map[string]CaseResultFile `json:"dependencyResults,omitempty"`
}

type RunRecord struct {
	ID                      string                `json:"id"`
	ScenarioID              string                `json:"scenarioId"`
	Namespace               string                `json:"namespace"`
	KubeContext             string                `json:"kubeContext"`
	DefaultStorageClass     string                `json:"defaultStorageClass,omitempty"`
	PreflightTools          []string              `json:"preflightTools,omitempty"`
	PreflightAt             time.Time             `json:"preflightAt,omitempty"`
	Release                 string                `json:"release"`
	Fullname                string                `json:"fullname"`
	ConfigSHA256            string                `json:"configSha256"`
	ScenarioFixtureSHA256   string                `json:"scenarioFixtureSha256,omitempty"`
	ScenarioValuesSHA256    string                `json:"scenarioValuesSha256,omitempty"`
	EnvironmentValuesSHA256 string                `json:"environmentValuesSha256,omitempty"`
	SourceSHA256            string                `json:"sourceSha256"`
	GitRevision             string                `json:"gitRevision,omitempty"`
	WorktreeStatus          string                `json:"worktreeStatus,omitempty"`
	ImageReference          string                `json:"imageReference,omitempty"`
	ImageDigest             string                `json:"imageDigest,omitempty"`
	ValuesFile              string                `json:"valuesFile,omitempty"`
	StartedAt               time.Time             `json:"startedAt"`
	FinishedAt              *time.Time            `json:"finishedAt,omitempty"`
	State                   string                `json:"state"`
	Attempt                 int                   `json:"attempt"`
	Failure                 string                `json:"failure,omitempty"`
	Mode                    string                `json:"mode,omitempty"`
	ExplicitCases           []string              `json:"explicitCases"`
	Plan                    []string              `json:"plan"`
	Cases                   map[string]CaseResult `json:"cases"`
	Resources               []ResourceIdentity    `json:"resources,omitempty"`
	CleanupError            string                `json:"cleanupError,omitempty"`
}

type CaseResult struct {
	Status       string        `json:"status"`
	StartedAt    time.Time     `json:"startedAt,omitempty"`
	FinishedAt   time.Time     `json:"finishedAt,omitempty"`
	Duration     time.Duration `json:"duration,omitempty"`
	Error        string        `json:"error,omitempty"`
	LogFile      string        `json:"logFile,omitempty"`
	OutputFile   string        `json:"outputFile,omitempty"`
	Requirements []string      `json:"requirements,omitempty"`
}

type ResourceIdentity struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	UID  string `json:"uid"`
}

type CaseResultFile struct {
	CaseID    string         `json:"caseId"`
	Status    string         `json:"status"`
	StartedAt time.Time      `json:"startedAt"`
	Finished  time.Time      `json:"finishedAt"`
	Checks    []CheckResult  `json:"checks,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
	Error     string         `json:"error,omitempty"`
}
