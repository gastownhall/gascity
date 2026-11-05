package extmsg

// safeOperationError preserves a cause for errors.Is/errors.As while keeping
// caller-controlled selectors and persistence identifiers out of Error text.
// The operation must be a constant, non-sensitive description.
type safeOperationError struct {
	operation string
	cause     error
}

func newSafeOperationError(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	return &safeOperationError{operation: operation, cause: cause}
}

func (e *safeOperationError) Error() string {
	return e.operation + " failed"
}

func (e *safeOperationError) Unwrap() error {
	return e.cause
}
