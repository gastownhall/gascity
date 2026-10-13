package beads

// BlockedAssignmentReleaser clears the assignee of raw blocked work and applies
// the supplied metadata patch in the same update, guarded by status and holder.
// Metadata keys merge; empty values clear lifecycle ties without deleting other
// work metadata. handled is false only when an exact raw
// read found open work still assigned to expectedAssignee, allowing its ordinary
// release policy. A missing, moved or other-status row, a lost blocked guard,
// and every refusal are handled: callers must never retry them as open work.
type BlockedAssignmentReleaser interface {
	ReleaseBlockedIfCurrent(id, expectedAssignee string, metadata map[string]string) (released, handled bool, err error)
}

// BlockedAssignmentReleaserFor follows the same declared provider boundaries
// as AssignmentGuardedUpdaterFor, without changing the rollout policy.
func BlockedAssignmentReleaserFor(store Store) (BlockedAssignmentReleaser, bool) {
	if store == nil {
		return nil, false
	}
	target := followConditionalWritesResolveTarget(store)
	if p, ok := target.(interface {
		blockedAssignmentReleaseHandle() (BlockedAssignmentReleaser, bool)
	}); ok {
		return p.blockedAssignmentReleaseHandle()
	}
	r, ok := target.(BlockedAssignmentReleaser)
	return r, ok
}
