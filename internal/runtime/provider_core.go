package runtime

import (
	"errors"
	"fmt"
)

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

// MergeBackendListResults merges provider ListRunning results and preserves
// best-effort behavior when only some backends fail.
//
// Historical callers assume ListRunning still returns any healthy backend's
// sessions so cleanup, discovery, and accounting can proceed. Only a total
// failure returns an error.
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
	if failed == len(results) {
		return nil, errors.Join(failures...)
	}
	return merged, nil
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
