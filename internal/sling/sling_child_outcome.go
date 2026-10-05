package sling

// Per-child outcomes of a convoy (batch) sling. Every child of an expanded
// container ends in exactly one of them; callers render or serialize the
// outcome instead of re-deriving it from SlingChildResult's flags.
const (
	// ChildOutcomeRouted is a child this sling routed (or attached a formula
	// to).
	ChildOutcomeRouted = "routed"
	// ChildOutcomeFailed is a child whose routing failed; FailReason says why.
	ChildOutcomeFailed = "failed"
	// ChildOutcomeSkipped is a child this sling left alone: already routed to
	// the target, or not open (Status names its status).
	ChildOutcomeSkipped = "skipped"
)

// Outcome names the child's outcome (ChildOutcomeRouted, ChildOutcomeFailed or
// ChildOutcomeSkipped).
func (c SlingChildResult) Outcome() string {
	switch {
	case c.Failed:
		return ChildOutcomeFailed
	case c.Routed:
		return ChildOutcomeRouted
	default:
		return ChildOutcomeSkipped
	}
}

// PartialFailure reports whether a batch sling both routed some children and
// failed others. Such a sling changed state, so retrying it as a whole is not
// safe: the caller needs the per-child outcomes to know what is left to do.
func (r SlingResult) PartialFailure() bool {
	return r.Routed > 0 && r.Failed > 0
}
