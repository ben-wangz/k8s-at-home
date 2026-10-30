package diagnostics

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"
)

const Prefix = "diagnostics/v1/"
const OutboxLimit = 32

type Failure struct {
	Class      string    `json:"class"`
	Summary    string    `json:"summary"`
	Stage      string    `json:"stage"`
	OccurredAt time.Time `json:"occurredAt"`
	AttemptID  string    `json:"attemptID,omitempty"`
}

func ObserveFailure(last **Failure, recovered *time.Time, class, summary, stage, attemptID string, at time.Time, success bool) {
	if class != "" {
		if *last == nil || (*last).Class != class || (*last).Stage != stage || (*last).AttemptID != attemptID || !recovered.IsZero() && !recovered.Before((*last).OccurredAt) {
			*last = &Failure{Class: class, Summary: summary, Stage: stage, AttemptID: attemptID, OccurredAt: at}
		}
	} else if success && *last != nil && recovered.Before((*last).OccurredAt) {
		*recovered = at
	}
}

type Event struct {
	ID                       string    `json:"id"`
	OccurredAt               time.Time `json:"occurredAt"`
	Component                string    `json:"component"`
	Operation                string    `json:"operation"`
	Stage                    string    `json:"stage"`
	Result                   string    `json:"result"`
	EntityID                 string    `json:"entityID,omitempty"`
	ImageID                  string    `json:"imageID,omitempty"`
	Authority                string    `json:"authority,omitempty"`
	Repository               string    `json:"repository,omitempty"`
	Digest                   string    `json:"digest,omitempty"`
	TaskID                   string    `json:"taskID,omitempty"`
	IntentID                 string    `json:"intentID,omitempty"`
	TransferID               string    `json:"transferID,omitempty"`
	WorkID                   string    `json:"workID,omitempty"`
	WorkKind                 string    `json:"workKind,omitempty"`
	RunID                    string    `json:"runID,omitempty"`
	Attempt                  uint64    `json:"attempt"`
	AttemptLayer             string    `json:"attemptLayer"`
	AttemptID                string    `json:"attemptID,omitempty"`
	ParentAttemptID          string    `json:"parentAttemptID,omitempty"`
	SourceRevision           string    `json:"sourceRevision,omitempty"`
	StartedAt                time.Time `json:"startedAt,omitempty"`
	FinishedAt               time.Time `json:"finishedAt,omitempty"`
	ErrorSummary             string    `json:"errorSummary,omitempty"`
	HTTPStatus               int       `json:"httpStatus,omitempty"`
	Retryable                bool      `json:"retryable"`
	ErrorClass               string    `json:"errorClass,omitempty"`
	Reason                   string    `json:"reason,omitempty"`
	TimeoutScope             string    `json:"timeoutScope"`
	Responsibility           string    `json:"responsibility"`
	NextAttemptAt            time.Time `json:"nextAttemptAt,omitempty"`
	RecoveredAt              time.Time `json:"recoveredAt,omitempty"`
	AdmissionWaitSeconds     *float64  `json:"admissionWaitSeconds,omitempty"`
	ExecutionDurationSeconds *float64  `json:"executionDurationSeconds,omitempty"`
	DeadlineRemainingSeconds *float64  `json:"deadlineRemainingSeconds,omitempty"`
	ExecutionBudgetSeconds   *float64  `json:"executionBudgetSeconds,omitempty"`
	RequestID                string    `json:"requestID,omitempty"`
	OperationID              string    `json:"operationID,omitempty"`
	S3RequestID              string    `json:"s3RequestID,omitempty"`
	TransportBytes           *int64    `json:"transportBytes,omitempty"`
	ExpectedBytes            *int64    `json:"expectedBytes,omitempty"`
	BodyComplete             *bool     `json:"bodyComplete,omitempty"`
}

func ID(parts ...string) string {
	data, _ := json.Marshal(parts)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func Record(outbox *[]Event, event Event) {
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	if event.TimeoutScope == "" {
		event.TimeoutScope = "unknown"
	}
	if event.AttemptLayer == "" {
		event.AttemptLayer = "source_transition"
	}
	if event.Responsibility == "" {
		event.Responsibility = "unknown"
	}
	if len(*outbox) > 0 {
		last := (*outbox)[len(*outbox)-1]
		sameEntity := last.Component == event.Component && last.Operation == event.Operation && last.EntityID == event.EntityID && last.WorkID == event.WorkID && last.TaskID == event.TaskID && last.IntentID == event.IntentID && last.TransferID == event.TransferID
		if sameEntity && last.RunID == event.RunID && last.AttemptID == event.AttemptID && last.SourceRevision == event.SourceRevision && last.Stage == event.Stage && last.Result == event.Result && last.Attempt == event.Attempt && last.ErrorClass == event.ErrorClass {
			return
		}
		if sameEntity && last.ErrorClass != "" && event.ErrorClass == "" && (event.Result == "done" || event.Result == "solidified" || event.Result == "anchored" || event.Result == "verified_local") {
			event.RecoveredAt = event.OccurredAt
		}
	}
	event.ID = ID(event.Component, event.EntityID, event.Stage, event.Result, strconv.FormatUint(event.Attempt, 10), event.OccurredAt.Format(time.RFC3339Nano))
	*outbox = append(*outbox, event)
	if len(*outbox) > OutboxLimit {
		*outbox = append([]Event(nil), (*outbox)[len(*outbox)-OutboxLimit:]...)
	}
}
