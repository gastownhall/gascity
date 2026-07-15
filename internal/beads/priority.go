package beads

// DefaultPriority is the priority band used when a bead has no explicit
// priority. It matches bd's COALESCE(priority, 2) ready-order contract.
const DefaultPriority = 2

// PriorityValue returns the numeric priority band, treating a missing value as
// P2. Lower numbers are more urgent: P0 precedes P1, then P2 through P4.
func PriorityValue(priority *int) int {
	if priority == nil {
		return DefaultPriority
	}
	return *priority
}

// ReadyLess reports whether a precedes b in the canonical work-admission
// order: priority band ascending, creation time ascending, then ID ascending.
func ReadyLess(a, b Bead) bool {
	pa, pb := PriorityValue(a.Priority), PriorityValue(b.Priority)
	if pa != pb {
		return pa < pb
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}
