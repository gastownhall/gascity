package beads

import (
	"errors"
	"fmt"
	"strings"
)

// ErrIssuePrefixMissing is the sentinel returned by translateBdError when
// the underlying bd error indicates the runtime config table lacks an
// `issue_prefix` row. Callers can use errors.Is to detect this specific
// failure mode programmatically — for example, to surface a custom
// retry-with-repair flow.
//
// The condition originates in #1232: bd 1.0.3+ rejects
// `bd config set issue_prefix`, the gc-beads-bd lifecycle script
// previously swallowed that rejection, and the resulting cities have a
// missing config row that breaks every `bd create`.
var ErrIssuePrefixMissing = errors.New("bd: issue_prefix config row missing — run `gc doctor --fix` to repair (issue #1232)")

// issuePrefixMissingMarker is the substring bd 1.x emits in stderr when
// the runtime `config` table lacks the `issue_prefix` row. Matching by
// substring rather than exact text tolerates bd minor-version output
// rephrasings as long as the error category remains the same.
const issuePrefixMissingMarker = "issue_prefix config is missing"

// translateBdError rewrites known bd error patterns into actionable gc
// errors. Currently translates the `issue_prefix config is missing`
// stderr text (from `bd create`) into ErrIssuePrefixMissing wrapped
// with the original error for context.
//
// Returns err unchanged when no pattern matches. Returns nil for nil
// input to keep the call site idiom `..., translateBdError(err)` safe.
func translateBdError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrIssuePrefixMissing) {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, issuePrefixMissingMarker) {
		return err
	}
	// Two %w verbs preserve both wrap targets so errors.Is matches both
	// the sentinel (for typed detection) and the original bd error (for
	// any deeper wrapping callers want to traverse).
	return fmt.Errorf("%w: %w", ErrIssuePrefixMissing, err)
}
