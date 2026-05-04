package runtime

import (
	"errors"
	"fmt"
)

// PartialListError reports that ListRunning returned best-effort results while
// one or more backends failed. Callers may continue using the returned names
// slice, but should surface the degraded backend error to operators.
type PartialListError struct {
	Err error
}

// Error returns the aggregated backend failure message.
func (e *PartialListError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

// Unwrap exposes the aggregated backend failure.
func (e *PartialListError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// BackendError carries provider/backend context for aggregated failures.
type BackendError struct {
	Label string
	Err   error
}

// BackendListResult captures one backend's ListRunning result.
type BackendListResult struct {
	Label string
	Names []string
	Err   error
}

// IsPartialListError reports whether err represents a degraded-but-usable
// ListRunning result from one or more failed backends.
func IsPartialListError(err error) bool {
	var target *PartialListError
	return errors.As(err, &target)
}

// DeadRuntimeSessionChecker is an optional provider capability for destructive
// cleanup paths that need positive proof a visible runtime artifact is dead.
// A false result means either the session is live, absent, or unsupported by
// the backend; a non-nil error means liveness could not be confirmed.
type DeadRuntimeSessionChecker interface {
	// IsDeadRuntimeSession reports whether name is visible but confirmed dead.
	IsDeadRuntimeSession(name string) (bool, error)
}

// MergeBackendListResults merges provider ListRunning results. On partial
// backend failure it returns the best-effort merged names plus a
// [PartialListError] so callers can continue with partial results while still
// surfacing backend degradation. Only a total failure returns no names.
func MergeBackendListResults(results ...BackendListResult) ([]string, error) {
	merged := make([]string, 0)
	failures := make([]error, 0, len(results))
	failed := 0

	for _, result := range results {
		merged = append(merged, result.Names...)
	}

	for _, result := range results {
		if result.Err == nil {
			continue
		}
		failed++
		failures = append(failures, fmt.Errorf("%s backend: %w", result.Label, result.Err))
	}

	if len(failures) == 0 {
		return merged, nil
	}
	if len(merged) > 0 {
		return merged, &PartialListError{Err: errors.Join(failures...)}
	}
	if failed == len(results) {
		return nil, errors.Join(failures...)
	}
	return merged, &PartialListError{Err: errors.Join(failures...)}
}

// MergeBackendStopErrors standardizes multi-backend Stop semantics.
// Any successful stop wins. If every backend reports the session as gone,
// Stop remains idempotent and returns nil.
func MergeBackendStopErrors(results ...BackendError) error {
	failures := make([]error, 0, len(results))
	allGone := len(results) > 0

	for _, result := range results {
		if result.Err == nil {
			return nil
		}
		if !IsSessionGone(result.Err) {
			allGone = false
		}
		failures = append(failures, fmt.Errorf("%s backend: %w", result.Label, result.Err))
	}

	if len(failures) == 0 || allGone {
		return nil
	}
	return errors.Join(failures...)
}
