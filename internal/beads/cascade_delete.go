package beads

// CascadeDeleter is an optional Store capability: remove many beads in one
// batched operation and let the backend's ON DELETE CASCADE drop their
// dependency, label, and event rows. A Store that implements it lets callers
// (notably the wisp GC) collapse an O(subprocess-per-edge) teardown of a
// molecule closure into a single batched delete on the sqlite/Dolt graph
// store. Callers that hold a plain Store fall back to per-bead deletion when
// the concrete store does not implement this interface.
type CascadeDeleter interface {
	// DeleteCascade removes ids and their edges as a batch. Implementations
	// tolerate ids that are already gone (idempotent) and may chunk internally
	// to respect backend limits.
	DeleteCascade(ids []string) error
}
