package api

import "fmt"

// partialAggregator collects errors from per-rig/per-backend operations
// that aggregate into a single list response. Handlers that previously
// did `if err != nil { continue }` now record the error through this
// helper so ListBody.Partial / ListBody.PartialErrors can surface the
// failure to clients instead of silently dropping a rig.
type partialAggregator struct {
	errs []string
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
