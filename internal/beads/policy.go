package beads

import "fmt"

// AdmissionPolicy names the order in which ready work is admitted to a worker.
// It governs ordering only: which ready bead a worker takes first. Eligibility
// — which beads are ready at all — is decided upstream by the work query and is
// never affected by this setting.
type AdmissionPolicy string

const (
	// PolicyPriorityFIFO admits the most urgent priority band first, oldest
	// first within a band. It is the default and the behavior of #4322.
	PolicyPriorityFIFO AdmissionPolicy = "priority_fifo"

	// PolicyFIFO admits the oldest ready bead first and ignores priority.
	PolicyFIFO AdmissionPolicy = "fifo"

	// DefaultAdmissionPolicy applies when a pool leaves the policy unset.
	DefaultAdmissionPolicy = PolicyPriorityFIFO
)

// Resolve returns the effective policy, mapping unset to DefaultAdmissionPolicy.
// Every admission path must resolve through this so an unset pool keeps the
// #4322 priority-band behavior.
func (p AdmissionPolicy) Resolve() AdmissionPolicy {
	if p == "" {
		return DefaultAdmissionPolicy
	}
	return p
}

// Validate reports whether p is a policy this build understands. Unset is valid
// and means DefaultAdmissionPolicy. Any other value is rejected rather than
// silently coerced, so a typo in pack.toml or city.toml fails fast instead of
// quietly reordering a fleet's work.
func (p AdmissionPolicy) Validate() error {
	switch p {
	case "", PolicyPriorityFIFO, PolicyFIFO:
		return nil
	default:
		return fmt.Errorf(
			"invalid work admission policy %q: must be %q or %q (unset means %q)",
			string(p), string(PolicyPriorityFIFO), string(PolicyFIFO), string(DefaultAdmissionPolicy),
		)
	}
}

// LessFunc returns the comparator implementing p. This is the single source of
// truth for admission order: every in-process path that sorts or selects ready
// work resolves its comparator here rather than hardcoding one.
func LessFunc(p AdmissionPolicy) func(a, b Bead) bool {
	if p.Resolve() == PolicyFIFO {
		return FIFOLess
	}
	return ReadyLess
}

// FIFOLess reports whether a precedes b in strict arrival order: creation time
// ascending, then ID ascending. Priority is deliberately ignored. The ID
// tie-break keeps the order total and deterministic across stores, matching
// ReadyLess's final tie-break.
func FIFOLess(a, b Bead) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}

// HasPriorityBands reports whether p treats priority as a hard band boundary.
//
// When true, a claim scan that has committed to a band must not fall through to
// a weaker one after losing a race (#4322's inversion guard). When false the
// policy has no bands at all, so the guard must stay inert: breaking the scan on
// a band change would skip older eligible work and stop the policy being FIFO.
func (p AdmissionPolicy) HasPriorityBands() bool {
	return p.Resolve() != PolicyFIFO
}

// JQSortKey returns the jq sort_by key list reproducing p. It must stay in
// lockstep with LessFunc; a divergence silently gives one admission path a
// different order from the rest (TestJQSortKeyMatchesComparator pins this).
//
// Shell paths order rows with this key and only THEN truncate to a window.
// Do not "optimize" that back into a `bd ready --sort ... --limit=N`: bd cannot
// express priority_fifo. Verified against beads v1.1.0
// (internal/storage/sqlbuild/ready.go, internal/storage/issueops/ready_work.go):
//
//	oldest    ORDER BY created ASC, id ASC                  == FIFOLess
//	priority  ORDER BY priority ASC, created DESC, id ASC   LIFO inside a band
//	hybrid    rows newer than 48h first; older rows take
//	          priority 999 (band discarded); then created ASC
//
// Only `oldest` matches a comparator of ours, and nothing matches ReadyLess. A
// bd-ordered window can therefore fill every slot with rows the policy ranks
// below an aged P0, which is then never fetched and starves under sustained
// fresh arrivals — the inversion this policy exists to prevent.
func (p AdmissionPolicy) JQSortKey() string {
	if p.Resolve() == PolicyFIFO {
		return `(.created_at // ""), (.id // "")`
	}
	return `(.priority // 2), (.created_at // ""), (.id // "")`
}
