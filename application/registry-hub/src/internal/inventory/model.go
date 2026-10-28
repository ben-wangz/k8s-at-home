package inventory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

const Prefix = "inventory/v1/"

type Identity struct {
	Authority  string `json:"authority"`
	Repository string `json:"repository"`
	Reference  string `json:"reference"`
}

func ID(kind string, parts ...string) string {
	data, _ := json.Marshal(append([]string{kind}, parts...))
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func (i Identity) ID() string { return ID("reference", i.Authority, i.Repository, i.Reference) }
func (i Identity) ContentID(digest string) string {
	return ID("content", i.Authority, i.Repository, digest)
}

type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

type ObjectRef struct {
	Role            string    `json:"role"`
	Digest          string    `json:"digest"`
	MediaType       string    `json:"mediaType"`
	Size            int64     `json:"size"`
	Platform        *Platform `json:"platform"`
	PlatformUnknown bool      `json:"platformUnknown,omitempty"`
	ArtifactType    string    `json:"artifactType,omitempty"`
}

type Content struct {
	SchemaVersion int    `json:"schemaVersion"`
	Source        string `json:"source"`
	Identity
	ObjectRef
	Generation   uint64      `json:"generation"`
	Children     []ObjectRef `json:"children"`
	ClosureKnown bool        `json:"closureKnown"`
	Subject      string      `json:"subject,omitempty"`
	UpdatedAt    time.Time   `json:"updatedAt"`
}

type Binding struct {
	SchemaVersion int `json:"schemaVersion"`
	Identity
	Digest            string    `json:"digest"`
	Generation        uint64    `json:"generation"`
	ContentGeneration uint64    `json:"contentGeneration"`
	Revision          string    `json:"revision"`
	Source            string    `json:"source"`
	ConfirmedAt       time.Time `json:"confirmedAt"`
}

type Lifecycle struct {
	SchemaVersion  int        `json:"schemaVersion"`
	Version        string     `json:"version"`
	Started        *bool      `json:"started"`
	FirstStartedAt *time.Time `json:"firstStartedAt"`
}

type Counts struct {
	Expected             *int64 `json:"expected"`
	Cached               int64  `json:"cached"`
	DistributionVerified int64  `json:"distributionVerified"`
	Missing              *int64 `json:"missing"`
	CleanupPending       int64  `json:"cleanupPending"`
}

type CacheState struct {
	State        string `json:"state"`
	Objects      Counts `json:"objects"`
	LogicalBytes Counts `json:"logicalBytes"`
}

type Availability struct {
	State string `json:"state"`
}

type Work struct {
	ID                 string     `json:"id"`
	Kind               string     `json:"kind"`
	Status             string     `json:"status"`
	WaitReason         string     `json:"waitReason,omitempty"`
	ErrorClass         string     `json:"errorClass,omitempty"`
	ErrorMessage       string     `json:"errorMessage,omitempty"`
	NextAttemptAt      *time.Time `json:"nextAttemptAt"`
	LastProgressAt     *time.Time `json:"lastProgressAt"`
	HistoryUnavailable bool       `json:"historyUnavailable"`
}

type Solidification struct {
	Lifecycle
	TaskCount      int        `json:"taskCount"`
	ExecutionState string     `json:"executionState"`
	LastProgressAt *time.Time `json:"lastProgressAt"`
	WaitReason     string     `json:"waitReason,omitempty"`
	NextAttemptAt  *time.Time `json:"nextAttemptAt"`
	Tasks          []Work     `json:"tasks"`
}

type Evidence struct {
	ClosureComplete bool      `json:"closureComplete"`
	CheckedAt       time.Time `json:"checkedAt"`
	Freshness       string    `json:"freshness"`
	Reasons         []string  `json:"reasons"`
	GCEpoch         uint64    `json:"gcEpoch"`
}

type Image struct {
	ID        string `json:"id"`
	ContentID string `json:"contentID"`
	Scope     string `json:"scope"`
	Identity
	ResolvedDigest     string         `json:"resolvedDigest"`
	BindingRevision    string         `json:"bindingRevision"`
	BindingSource      string         `json:"bindingSource"`
	BindingConfirmedAt time.Time      `json:"bindingConfirmedAt"`
	Generation         uint64         `json:"generation"`
	ContentGeneration  uint64         `json:"contentGeneration"`
	MediaType          string         `json:"mediaType"`
	Kind               string         `json:"kind"`
	ArtifactType       string         `json:"artifactType,omitempty"`
	Subject            string         `json:"subject,omitempty"`
	Platform           *Platform      `json:"platform"`
	Category           string         `json:"category"`
	Cache              CacheState     `json:"cache"`
	Availability       Availability   `json:"availability"`
	Solidification     Solidification `json:"solidification"`
	Evidence           Evidence       `json:"evidence"`
	TagPublished       bool           `json:"tagPublished"`
}

type Object struct {
	ID string `json:"id"`
	Identity
	ObjectRef
	Generation           uint64    `json:"generation"`
	CacheState           string    `json:"cacheState"`
	DataVersion          string    `json:"dataVersion,omitempty"`
	CleanupPending       bool      `json:"cleanupPending"`
	DistributionVerified bool      `json:"distributionVerified"`
	Anchored             bool      `json:"anchored"`
	CheckedAt            time.Time `json:"checkedAt"`
	Reasons              []string  `json:"reasons"`
	Associated           bool      `json:"associated"`
	IntentID             string    `json:"intentID,omitempty"`
	IntentState          string    `json:"intentState,omitempty"`
	TaskIDs              []string  `json:"taskIDs"`
}
