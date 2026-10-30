package main

import "github.com/gastownhall/gascity/internal/beads"

var _ beads.GuardedBatcherHandleProvider = (*beadPolicyStore)(nil)

// GuardedBatcherHandle opts the policy wrapper in to guarded-batch discovery:
// a batch goes to the wrapped store's batcher. The wrapper's storage-tier
// policy shapes Create, not a guarded create, so a caller writing journal rows
// through a batch sets NoHistory and Ephemeral itself. beadPolicyGraphStore
// inherits this through its embedded *beadPolicyStore.
func (s *beadPolicyStore) GuardedBatcherHandle() (beads.GuardedBatcher, bool) {
	return beads.GuardedBatcherFor(s.Store)
}
