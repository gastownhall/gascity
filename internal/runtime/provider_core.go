package runtime

import (
	"errors"
	"fmt"
	"strings"
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
// backend context when one or more providers fail.
func MergeBackendListResults(results ...BackendListResult) ([]string, error) {
	merged := make([]string, 0)
	successLabels := make([]string, 0, len(results))
	failures := make([]error, 0, len(results))
	failed := 0

	for _, result := range results {
		merged = append(merged, result.Names...)
		if result.Err == nil {
			successLabels = append(successLabels, result.Label)
		}
	}

	for _, result := range results {
		if result.Err == nil {
			continue
		}
		failed++
		err := fmt.Errorf("%s backend: %w", result.Label, result.Err)
		if note := partialResultsNote(result.Label, successLabels); note != "" {
			err = fmt.Errorf("%s backend: %w (%s)", result.Label, result.Err, note)
		}
		failures = append(failures, err)
	}

	if len(failures) == 0 {
		return merged, nil
	}
	if failed == len(results) {
		return nil, errors.Join(failures...)
	}
	return merged, errors.Join(failures...)
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

func partialResultsNote(failedLabel string, successLabels []string) string {
	included := make([]string, 0, len(successLabels))
	for _, label := range successLabels {
		if label == failedLabel {
			continue
		}
		included = append(included, label)
	}
	if len(included) == 0 {
		return ""
	}
	return fmt.Sprintf("%s results included", strings.Join(included, " + "))
}
