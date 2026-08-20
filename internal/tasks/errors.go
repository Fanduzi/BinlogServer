// Package tasks provides module-level functionality for tasks.
// input: runner/source failure strings and typed operator error codes
// output: permanent (non-retryable) errors that stop the task state machine
// pos: shared error types used by scheduler retry policy and source probing
// note: if this file changes, update this header and module README.md.
package tasks

import "errors"

const (
	// CodeSourceAccessDenied is MySQL/MariaDB ERROR 1045.
	CodeSourceAccessDenied = "SOURCE_ACCESS_DENIED"
	// CodeSourceLogBinOff is an unrecoverable source configuration error.
	CodeSourceLogBinOff = "SOURCE_LOG_BIN_OFF"
	// CodeSourceIdentityUnavailable is a missing source identity after probing.
	CodeSourceIdentityUnavailable = "SOURCE_IDENTITY_UNAVAILABLE"
	// CodeInvalidRequest is a create/update validation failure.
	CodeInvalidRequest = "INVALID_REQUEST"
)

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
