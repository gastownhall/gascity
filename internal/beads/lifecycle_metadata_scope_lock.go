package beads

// lockLifecycleMetadata shares the same scope-wide lease as close mutations.
// The bead ID remains in the signature because it is part of the capability's
// public operation, but the lock must span the whole scope to avoid close vs
// update lock-order inversions in synchronous bd hooks.
func lockLifecycleMetadata(scope, _ string) (unlock func(), err error) {
	lease, err := acquireLifecycleMutationLease(scope, inheritedLifecycleMutationFromEnv())
	if err != nil {
		return nil, err
	}
	return lease.Unlock, nil
}

// lockProcessLifecycleMetadata takes only the process half of the unified
// lifecycle lease. FileStore uses this for its canonical file-path domain and
// then takes its existing per-file Locker for the cross-process half. Passing
// the JSON file path to acquireLifecycleMutationLease would incorrectly try to
// open that regular file as a directory-backed beads scope.
func lockProcessLifecycleMetadata(scope, _ string) (unlock func(), err error) {
	lease, err := acquireFreshLifecycleMutationLease("", closeTransitionScopeKey(scope), nil)
	if err != nil {
		return nil, err
	}
	return lease.Unlock, nil
}
