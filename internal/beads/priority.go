package beads

// DefaultPriority is the priority band used when a bead has no explicit
// priority. It matches bd's COALESCE(priority, 2) ready-order contract.
const (
	DefaultPriority     = 2
	LeastUrgentPriority = 4
	InvalidPriorityRank = LeastUrgentPriority + 1
)

// PriorityValue returns the numeric priority rank, treating a missing value as
// P2 and an out-of-range value as less urgent than P4. Lower ranks are more
// urgent: P0 precedes P1, then P2 through P4.
func PriorityValue(priority *int) int {
	if priority == nil {
		return DefaultPriority
	}
	if *priority < 0 || *priority > LeastUrgentPriority {
		return InvalidPriorityRank
	}
	return *priority
}

// ReadyLess reports whether a precedes b in canonical work-admission order:
// priority band ascending, creation time ascending, then ID ascending.
func ReadyLess(a, b Bead) bool {
	pa, pb := PriorityValue(a.Priority), PriorityValue(b.Priority)
	if pa != pb {
		return pa < pb
	}
	if a.CreatedAt.IsZero() != b.CreatedAt.IsZero() {
		return !a.CreatedAt.IsZero()
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}
