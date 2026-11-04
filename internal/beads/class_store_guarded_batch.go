package beads

// The typed class wrappers opt in to guarded-batch discovery: each is a view
// over the store it embeds, with no write policy of its own, so a guarded
// batch through it is a guarded batch on that store.

var (
	_ GuardedBatcherHandleProvider = WorkStore{}
	_ GuardedBatcherHandleProvider = GraphStore{}
	_ GuardedBatcherHandleProvider = SessionStore{}
	_ GuardedBatcherHandleProvider = MailStore{}
	_ GuardedBatcherHandleProvider = OrdersStore{}
	_ GuardedBatcherHandleProvider = NudgesStore{}
)

// GuardedBatcherHandle forwards guarded-batch discovery to the wrapped store.
func (s WorkStore) GuardedBatcherHandle() (GuardedBatcher, bool) { return GuardedBatcherFor(s.Store) }

// GuardedBatcherHandle forwards guarded-batch discovery to the wrapped store.
func (s GraphStore) GuardedBatcherHandle() (GuardedBatcher, bool) { return GuardedBatcherFor(s.Store) }

// GuardedBatcherHandle forwards guarded-batch discovery to the wrapped store.
func (s SessionStore) GuardedBatcherHandle() (GuardedBatcher, bool) {
	return GuardedBatcherFor(s.Store)
}

// GuardedBatcherHandle forwards guarded-batch discovery to the wrapped store.
func (s MailStore) GuardedBatcherHandle() (GuardedBatcher, bool) { return GuardedBatcherFor(s.Store) }

// GuardedBatcherHandle forwards guarded-batch discovery to the wrapped store.
func (s OrdersStore) GuardedBatcherHandle() (GuardedBatcher, bool) {
	return GuardedBatcherFor(s.Store)
}

// GuardedBatcherHandle forwards guarded-batch discovery to the wrapped store.
func (s NudgesStore) GuardedBatcherHandle() (GuardedBatcher, bool) {
	return GuardedBatcherFor(s.Store)
}
