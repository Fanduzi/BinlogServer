// Package tasks provides module-level functionality for tasks.
// input: runner/source failures and stable operator error codes
// output: typed permanent/retryable source errors, SEALED_FILE_EXISTS and CHECKPOINT_WRITE_FAILED, the SOURCE_UNREACHABLE budget predicate, the retry allowlist (SOURCE_UNREACHABLE, transient metadata text, OBJECT_PURGE_FAILED), a lease-handoff error that must not be written as FAILED, EPOCH_NOT_ACQUIRED when a cluster runner is asked to run at epoch 0, and SEGMENT_NOT_ON_WORKER for a takeover segment that is not on this worker
// pos: shared operator-error types used by scheduler retry policy and source probing
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"errors"
	"fmt"
	"strings"
)

const (
	// CodeSourceAccessDenied is MySQL/MariaDB ERROR 1045.
	CodeSourceAccessDenied = "SOURCE_ACCESS_DENIED"
	// CodeSourceUnreachable is a retryable source network failure.
	CodeSourceUnreachable = "SOURCE_UNREACHABLE"
	// CodeSourceLogBinOff is an unrecoverable source configuration error.
	CodeSourceLogBinOff = "SOURCE_LOG_BIN_OFF"
	// CodeSourceIdentityUnavailable is a missing source identity after probing.
	CodeSourceIdentityUnavailable = "SOURCE_IDENTITY_UNAVAILABLE"
	// CodeInvalidRequest is a create/update validation failure.
	CodeInvalidRequest = "INVALID_REQUEST"
	// CodeSegmentNotOnWorker means takeover cannot read the previous worker's segment.
	// The task stays FAILED until that file is readable here.
	CodeSegmentNotOnWorker = "SEGMENT_NOT_ON_WORKER"
	// CodeSealedFileExists means the rotate target is already sealed on this disk.
	// Another attempt will hit the same file. The task stays FAILED until an operator removes the conflict.
	CodeSealedFileExists = "SEALED_FILE_EXISTS"
	// CodeCheckpointWriteFailed means the checkpoint store rejected a write for a reason that will not clear on its own.
	CodeCheckpointWriteFailed = "CHECKPOINT_WRITE_FAILED"
	// CodeEpochNotAcquired means a cluster runner was handed epoch 0.
	// The dump does not start. Start the task again.
	CodeEpochNotAcquired = "EPOCH_NOT_ACQUIRED"
)

// ErrLeaseHandoff means this runner's lease epoch is no longer the one that owns the task.
// The scheduler stops the runner and releases only this epoch. It does not write FAILED or RETRY_BACKOFF.
var ErrLeaseHandoff = errors.New("lease handed off")

// NewLeaseHandoff marks err as a lease handoff. A nil err is ErrLeaseHandoff itself.
func NewLeaseHandoff(err error) error {
	if err == nil || errors.Is(err, ErrLeaseHandoff) {
		return ErrLeaseHandoff
	}
	return fmt.Errorf("%w: %w", ErrLeaseHandoff, err)
}

// IsLeaseHandoff reports whether this runner must stop without writing the task row.
// The replication sentinel text is accepted so a runner that has not wrapped it still hands off.
func IsLeaseHandoff(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrLeaseHandoff) {
		return true
	}
	msg := err.Error()
	return msg == "lease/epoch mismatch" || strings.HasSuffix(msg, ": lease/epoch mismatch")
}

// RetryableSourceError is a source failure that may recover without operator action.
type RetryableSourceError struct {
	Code    string
	Message string
}

func (e *RetryableSourceError) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// NewRetryableSourceError constructs a retryable operator-facing source error.
func NewRetryableSourceError(code, message string) error {
	return &RetryableSourceError{Code: code, Message: message}
}

// IsSourceUnreachable reports whether err consumes the unreachable-source retry budget.
func IsSourceUnreachable(err error) bool {
	var sourceErr *RetryableSourceError
	return errors.As(err, &sourceErr) && sourceErr.Code == CodeSourceUnreachable
}

// IsTransientMetadataError reports a metadata blip that must not fail the task.
// meta.IsTransientMySQLError calls this so the substring list stays in one place:
// deadlock, lock wait timeout, connection reset, connection refused, broken pipe,
// server has gone away, invalid connection, bad connection, read-only, read only,
// timeout, eof. There is no task-level cap.
func IsTransientMetadataError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "deadlock") ||
		strings.Contains(msg, "lock wait timeout") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "server has gone away") ||
		strings.Contains(msg, "invalid connection") ||
		strings.Contains(msg, "bad connection") ||
		strings.Contains(msg, "read-only") ||
		strings.Contains(msg, "read only") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "eof")
}

// isObjectPurgeFailed reports a retention delete the next file open retries.
// It is not a task failure.
func isObjectPurgeFailed(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "OBJECT_PURGE_FAILED")
}

// retryAllowed is the fail-and-alert allowlist. A lease handoff is not a retry
// and not a failure. Anything else fails the task on the first occurrence.
func retryAllowed(err error) bool {
	if err == nil || IsPermanent(err) || IsLeaseHandoff(err) {
		return false
	}
	return IsSourceUnreachable(err) || IsTransientMetadataError(err) || isObjectPurgeFailed(err)
}

// PermanentError is an unrecoverable task error that must not enter RETRY_BACKOFF.
type PermanentError struct {
	Code    string
	Message string
}

func (e *PermanentError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// NewPermanentError constructs an unrecoverable operator-facing error.
func NewPermanentError(code, message string) error {
	return &PermanentError{Code: code, Message: message}
}

// IsPermanent reports whether err (or any wrapped error) is unrecoverable.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// classifyRunError turns local conditions that need an operator into permanent errors.
// A lease handoff and an already-typed source error are left unchanged.
func classifyRunError(err error) error {
	if err == nil || IsPermanent(err) || IsSourceUnreachable(err) || IsLeaseHandoff(err) {
		return err
	}
	if strings.Contains(err.Error(), "sealed file already exists:") {
		return NewPermanentError(CodeSealedFileExists, err.Error())
	}
	return err
}
