package beads

// NewNativeDoltStoreForConformance returns a NativeDoltStore backed by the
// in-memory native storage fixture for the external conformance suite.
func NewNativeDoltStoreForConformance() Store {
	return newNativeDoltStoreForTest(newNativeDoltMemStorage())
}

// NewNativeDoltStoreForPinnedIDFenceConformance returns a NativeDoltStore
// minting under mintPrefix and fenced to exactly the given namespaces, for
// beadstest.RunPinnedIDFenceConformance.
//
// namespaces is forwarded VERBATIM to the same option production wiring uses,
// with no branch on emptiness — the suite needs an empty set to reach
// production's unfenced case, which is the control that tells a fence from a
// blanket refusal.
//
// The backing storage honors explicit ids so a pinned id survives the round
// trip; without that the fixture would clobber every id the suite pins and the
// rows would pass against a store that never had to decide anything.
func NewNativeDoltStoreForPinnedIDFenceConformance(mintPrefix string, namespaces ...string) Store {
	storage := &nativeDoltMemStorage{store: &MemStore{IDPrefix: mintPrefix, HonorExplicitIDs: true}}
	store := newNativeDoltStoreForTest(storage, WithNativeDoltStoreReservedIDPrefixes(namespaces...))
	store.idPrefix = normalizeIDPrefix(mintPrefix)
	return store
}

// NotifyChangeForTest drives the real producer (CachingStore.notifyChange) with
// a caller-supplied bead, bypassing the store-write path that rewrites ids and
// status. It lets cross-package guardrail tests (e.g. the run-view round-trip)
// emit an exact run-shaped bead through the production event-marshal + run/session
// id-resolution seam. The onChange callback receives the same 6-tuple the record
// site (cmd/gc/api_state.go) wraps into an events.Event.
func (c *CachingStore) NotifyChangeForTest(eventType string, b Bead) {
	c.notifyChange(eventType, b)
}

// NewProxiedStoreForConformance returns the proxied-native SPLIT store — native
// read leaf, bd-shaped write leaf — over one in-memory ledger, for the external
// conformance suite.
//
// Three things about the fixture are load-bearing.
//
// Both leaves share ONE storage, because a conformance suite writes and then
// reads back: two leaves over two ledgers would pass nothing, and two leaves over
// one ledger is exactly the production shape (bd's CLI and gc's library handle
// are two doors onto the same Dolt database).
//
// The read leaf is READ-ONLY LATCHED, exactly as the proxied opener latches it.
// That turns every routing mistake into a loud failure: a write that leaked to
// the read leaf refuses with ErrProxiedNativeReadOnly instead of quietly
// succeeding against the same ledger and proving nothing.
//
// The write leaf stands in for *BdStore rather than being one, because a real
// BdStore forks the `bd` binary. It is a second NativeDoltStore over the same
// storage, which is the closest in-process thing to "the other door".
func NewProxiedStoreForConformance(mintPrefix string) Store {
	storage := &nativeDoltMemStorage{store: &MemStore{IDPrefix: mintPrefix, HonorExplicitIDs: true}}

	native := newNativeDoltStoreForTest(storage, WithProxiedReadOnly())
	native.idPrefix = normalizeIDPrefix(mintPrefix)
	write := newNativeDoltStoreForTest(storage)
	write.idPrefix = normalizeIDPrefix(mintPrefix)

	store, err := NewProxiedStore(native, write, PinForTest("/scope", "/scope/.beads/dolt", "beads"))
	if err != nil {
		panic("beads: building the conformance proxied store: " + err.Error())
	}
	return store
}

// PinForTest mints an admitted Pin without running admission.
//
// Pin's fields are unexported and only Admit sets admitted, which is the fence
// that keeps the proxied opener unreachable without a gate pass. A test that
// needed a real admission pass would need a real proxy record, a real process
// table and a real TCP probe, so the fence gets one in-package door and it is
// _test-only: export_test.go is compiled into the package's own test binary and
// into nothing else.
func PinForTest(scopeRoot, root, database string) Pin {
	return Pin{admitted: true, scopeRoot: scopeRoot, root: root, database: database}
}
