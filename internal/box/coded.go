package box

import (
	"errors"
	"fmt"
)

// Error codes of the runtime contract (docs/runtime-contract.md). A refusal
// carries one so a caller can act on it without reading the message.
const (
	CodeDockerUnavailable = "docker_unavailable"
	CodeImageMismatch     = "image_mismatch"
	CodeAlreadyRunning    = "box_already_running"
	CodeNotRunning        = "box_not_running"
)

// CodedError is an error with a contract error code. Its message is the
// usual plain-English one.
type CodedError struct {
	Code string
	Err  error
}

func (e CodedError) Error() string { return e.Err.Error() }
func (e CodedError) Unwrap() error { return e.Err }

// coded makes a CodedError from a message.
func coded(code, format string, a ...any) error {
	return CodedError{Code: code, Err: fmt.Errorf(format, a...)}
}

// CodeOf is the contract code an error carries, or "" for none.
func CodeOf(err error) string {
	var c CodedError
	if errors.As(err, &c) {
		return c.Code
	}
	var n ErrNotUp
	if errors.As(err, &n) {
		return CodeNotRunning
	}
	return ""
}
