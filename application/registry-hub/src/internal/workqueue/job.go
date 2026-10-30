package workqueue

import (
	"encoding/json"
	"registry-hub/internal/diagnostics"
	"time"
)

type Job struct {
	DiagnosticOutbox         []diagnostics.Event `json:"diagnosticOutbox,omitempty"`
	SchemaVersion            int                 `json:"schemaVersion"`
	ID                       string              `json:"id"`
	Kind                     string              `json:"kind"`
	Payload                  json.RawMessage     `json:"payload"`
	Progress                 json.RawMessage     `json:"progress,omitempty"`
	State                    string              `json:"state"`
	CreatedAt                time.Time           `json:"createdAt"`
	EnqueuedAt               time.Time           `json:"enqueuedAt,omitempty"`
	RecoveredAt              time.Time           `json:"recoveredAt,omitempty"`
	RetryRequestID           string              `json:"retryRequestID,omitempty"`
	RetryAcceptedAt          time.Time           `json:"retryAcceptedAt,omitempty"`
	RunID                    string              `json:"runID,omitempty"`
	LeaseOwner               string              `json:"leaseOwner,omitempty"`
	PreviousLeaseOwner       string              `json:"previousLeaseOwner,omitempty"`
	AttemptID                string              `json:"attemptID,omitempty"`
	ParentAttemptID          string              `json:"parentAttemptID,omitempty"`
	FirstAttemptAt           time.Time           `json:"firstAttemptAt,omitempty"`
	LastAttemptAt            time.Time           `json:"lastAttemptAt,omitempty"`
	AttemptFinishedAt        time.Time           `json:"attemptFinishedAt,omitempty"`
	CompletedAt              time.Time           `json:"completedAt,omitempty"`
	RunnableAt               time.Time           `json:"runnableAt,omitempty"`
	WaitReason               string              `json:"waitReason,omitempty"`
	ResponsibleWorkID        string              `json:"responsibleWorkID,omitempty"`
	ResponsibleWorkKind      string              `json:"responsibleWorkKind,omitempty"`
	Superseded               bool                `json:"superseded,omitempty"`
	Checkpoint               Checkpoint          `json:"checkpoint"`
	AdmissionWaitSeconds     float64             `json:"admissionWaitSeconds"`
	ExecutionDurationSeconds float64             `json:"executionDurationSeconds"`
	ExecutionBudgetSeconds   float64             `json:"executionBudgetSeconds"`
	TimeoutScope             string              `json:"timeoutScope,omitempty"`
	ProgressAt               time.Time           `json:"progressAt"`
	LeaseUntil               time.Time           `json:"leaseUntil,omitempty"`
	Fence                    uint64              `json:"fence"`
	RetryCount               int                 `json:"retryCount"`
	NextAttemptAt            time.Time           `json:"nextAttemptAt,omitempty"`
	ErrorAt                  time.Time           `json:"errorAt,omitempty"`
	ErrorClass               string              `json:"errorClass,omitempty"`
	Phase                    int                 `json:"phase"`
	ErrorPhase               int                 `json:"errorPhase"`
	Version                  string              `json:"-"`
}
