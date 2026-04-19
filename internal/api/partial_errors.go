package api

import (
	"fmt"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// partialAggregator collects errors from per-rig/per-backend operations
// that aggregate into a single list response. Handlers that previously
// did `if err != nil { continue }` now record the error through this
// helper so ListBody.Partial / ListBody.PartialErrors can surface the
// failure to clients instead of silently dropping a rig.
//
// It also tracks how many backends attempted the operation and how many
// of those succeeded, so handlers can fail hard (503) when every
// backend errored instead of synthesizing a 200 + empty list that looks
// indistinguishable from "no data."
type partialAggregator struct {
	errs      []string
	attempts  int
	successes int
}

// attempt records that a backend attempt was made (whether it succeeded
// or not). Call before calling record / success.
func (p *partialAggregator) attempt() {
	p.attempts++
}

// success records a successful backend call.
func (p *partialAggregator) success() {
	p.successes++
}

// record appends a per-rig error. label is a short stable identifier
// (usually the rig name). The raw error message is included so operators
// can diagnose; no stack traces or sensitive data leak because callers
// already construct these errors with identifying context.
func (p *partialAggregator) record(label string, err error) {
	if err == nil {
		return
	}
	p.errs = append(p.errs, fmt.Sprintf("%s: %v", label, err))
}

// partial reports whether any error has been recorded.
func (p *partialAggregator) partial() bool {
	return len(p.errs) > 0
}

// messages returns the accumulated messages (nil if none).
func (p *partialAggregator) messages() []string {
	if len(p.errs) == 0 {
		return nil
	}
	out := make([]string, len(p.errs))
	copy(out, p.errs)
	return out
}

// totalOutage reports whether every attempted backend failed. Callers
// check this before returning a 200 + empty list; when totalOutage is
// true the right response is a 503 with the aggregated errors so
// clients can tell "everything is down" from "there is no data."
func (p *partialAggregator) totalOutage() bool {
	return p.attempts > 0 && p.successes == 0
}

// outageError returns a 503 Problem Details error carrying the
// aggregated per-backend messages, suitable for direct return from a
// Huma handler when totalOutage() is true.
func (p *partialAggregator) outageError() error {
	detail := "all backends failed"
	if msgs := p.messages(); len(msgs) > 0 {
		detail = detail + ": " + strings.Join(msgs, "; ")
	}
	return huma.Error503ServiceUnavailable(detail)
}
