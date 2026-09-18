package observability

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Stable error codes used in the errorCode log field and exit-code mapping.
const (
	CodeConfigInvalid      = "config_invalid"
	CodeInputInvalid       = "input_invalid"
	CodeURLInvalid         = "url_invalid"
	CodeHostKeyFailed      = "host_key_verification_failed"
	CodeAuthFailed         = "authentication_failed"
	CodeRepoUnavailable    = "repository_unavailable"
	CodeGitFailed          = "git_failed"
	CodeGitTimeout         = "git_timeout"
	CodeArchiveFailed      = "archive_failed"
	CodeStorageFailed      = "storage_failed"
	CodePublishConflict    = "publish_conflict"
	CodePublishUnknown     = "publication_unknown"
	CodeRetentionFailed    = "retention_failed"
	CodeCapacity           = "capacity_insufficient"
	CodeCapabilityMissing  = "filesystem_capability_missing"
	CodeDeadlineExceeded   = "deadline_exceeded"
	CodeCanceled           = "canceled"
	CodePrepareFailed      = "prepare_failed"
	CodeObjectCorrupt      = "object_state_conflict"
	CodeFilesystemFailed   = "filesystem_error"
	CodeUnsupportedFeature = "unsupported_feature"
)

// SafeError carries a stable code plus a message that is safe to log: it must
// not contain repository URLs, credentials, raw YAML, or remote stderr. The
// wrapped cause may hold sensitive context for internal handling only.
type SafeError struct {
	Code string
	Msg  string
	Err  error
}

func (e *SafeError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Msg, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Msg)
}

func (e *SafeError) Unwrap() error { return e.Err }

// NewSafe builds a SafeError. msg must already be sanitized.
func NewSafe(code, msg string) *SafeError { return &SafeError{Code: code, Msg: msg} }

// WrapSafe builds a SafeError wrapping err.
func WrapSafe(code, msg string, err error) *SafeError {
	return &SafeError{Code: code, Msg: msg, Err: err}
}

// errCodeUnknown is the fallback code for unclassified errors.
const errCodeUnknown = "internal_error"

// CodeOf returns the error code for err, defaulting to errCodeUnknown.
func CodeOf(err error) string {
	var se *SafeError
	if errors.As(err, &se) {
		return se.Code
	}
	if errors.Is(err, context.Canceled) {
		return CodeCanceled
	}
	return errCodeUnknown
}

// ClassifyGitStderr maps bounded Git/OpenSSH stderr text to a stable error
// code. The text itself is never embedded in the returned message.
func ClassifyGitStderr(stderr string, timedOut bool) string {
	if timedOut {
		return CodeGitTimeout
	}
	s := strings.ToLower(stderr)
	switch {
	case strings.Contains(s, "host key verification failed"),
		strings.Contains(s, "host key verification failure"):
		return CodeHostKeyFailed
	case strings.Contains(s, "permission denied"),
		strings.Contains(s, "authentication failed"),
		strings.Contains(s, "no more authentication methods"),
		strings.Contains(s, "identification of the remote side failed"),
		strings.Contains(s, "could not read username"),
		strings.Contains(s, "terminal prompts disabled"),
		strings.Contains(s, "authentication required"),
		strings.Contains(s, "unauthorized"),
		strings.Contains(s, "invalid credentials"),
		strings.Contains(s, "invalid username or password"),
		strings.Contains(s, "http basic: access denied"),
		strings.Contains(s, "returned error: 401"),
		strings.Contains(s, "returned error: 403"):
		return CodeAuthFailed
	case strings.Contains(s, "could not read from remote repository"),
		strings.Contains(s, "repository not found"),
		strings.Contains(s, "does not appear to be a git repository"),
		strings.Contains(s, "connection refused"),
		strings.Contains(s, "connection timed out"),
		strings.Contains(s, "timed out"),
		strings.Contains(s, "no route to host"),
		strings.Contains(s, "name or service not known"):
		return CodeRepoUnavailable
	}
	return CodeGitFailed
}
