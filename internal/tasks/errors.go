// Package tasks provides module-level functionality for tasks.
// input: runner/source failures and stable operator error codes
// output: typed permanent and retryable source errors used by task retry policy
// pos: shared operator-error types used by scheduler retry policy and source probing
// note: if this file changes, update this header and module README.md.
package tasks

import "errors"

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
)

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

// IsRetryableSourceError reports whether err is a classified retryable source failure.
func IsRetryableSourceError(err error) bool {
	var sourceErr *RetryableSourceError
	return errors.As(err, &sourceErr)
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
