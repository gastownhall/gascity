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
	// ServerAbsent reports that the backend's runtime server was not running
	// at all. An absent server is still a failed observation, not proof that
	// zero sessions exist (see gastownhall/gascity#4082), so this never
	// relaxes the fail-safe on its own - it only lets callers holding
	// independent proof of death distinguish the two failure shapes.
	//
	// Never set on a merged multi-backend result: the absence of one backend
	// says nothing about its siblings.
	ServerAbsent bool
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

// IsRuntimeServerAbsent reports whether err is a [PartialListError] whose
// consulted backend was not running at all, as opposed to a server that was up
// and answered incompletely. Reap paths holding independent proof of death use
// it to act instead of deferring forever.
//
// It deliberately does NOT unwrap: only an error returned directly by a single
// backend can assert absence. A composite provider joins its backends' errors
// (and returns a bare join when every backend fails), so any traversing check
// would report absence whenever ANY backend was absent - including when a
// sibling backend is healthy, or merely failed for an unrelated reason, while
// still holding live sessions. Wrapping therefore degrades to "not absent",
// which is the fail-safe answer.
func IsRuntimeServerAbsent(err error) bool {
	target, ok := err.(*PartialListError) //nolint:errorlint // the non-traversing assertion is the point: see the doc comment above — errors.As would report absence whenever ANY joined backend was absent, including when a healthy sibling still holds live sessions
	return ok && target.ServerAbsent
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
