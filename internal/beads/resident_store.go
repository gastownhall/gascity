package beads

// ResidentStore is the writable handle on the store that holds a bead of any
// class (typed-stores design §3.6, T7a): the owner a by-id resolution proved,
// a leg a walk stopped on, or one leg of a write sweep. It is today's handle
// shape, the store embedded, and like the class handles it promotes every
// Store method, is a class layer that As and SameStore see through, declares
// the store as its conditional-writes resolution target, and forwards the
// handle providers. Only the residency layer mints one (MintResident,
// MintResidentLeg; beadstest in tests).
//
// The split-strict gate polices its reads (OD-T7-3) unless it was minted over
// a leg layer (MintResidentLeg). Then its Store-method bead reads carry the
// leg's role, and so do the reads below the leg that go through a
// role-carrying adapter (leg_reads.go): HandlesFor's and LiveFor's readers,
// GetMany, and As's CachedLister and Counter. Any other capability's reads
// carry none, and are policed.
type ResidentStore struct {
	Store
	ref string
}

// MintResident is the resident handle over s, named ref (the residency
// layer's store-ref vocabulary). It is a mint: the storeclass lint reports any
// caller but internal/storeref, and beadstest.Resident in tests.
func MintResident(s Store, ref string) ResidentStore { return ResidentStore{Store: s, ref: ref} }

// MintResidentLeg is the writable handle over r's leg layer, for a sweep that
// writes through the legs it reads: its reads keep the leg's role, its
// writes go to the leg's store. A zero reader mints a zero handle. A mint, as
// MintResident.
func MintResidentLeg(r FederatedReader) ResidentStore {
	if r.leg == nil {
		return ResidentStore{}
	}
	return ResidentStore{Store: r.leg, ref: r.leg.legRef()}
}

// Ref names the store the handle wraps.
func (s ResidentStore) Ref() string { return s.ref }

// Raw returns the store the handle wraps; see WorkStore.Raw.
func (s ResidentStore) Raw() Store { return s.Store }

// ConditionalWritesResolveTarget declares the wrapped store as the
// conditional-writes resolution target.
func (s ResidentStore) ConditionalWritesResolveTarget() Store { return s.Store }

// GuardedBatcherHandle forwards guarded-batch discovery to the wrapped store.
func (s ResidentStore) GuardedBatcherHandle() (GuardedBatcher, bool) {
	return GuardedBatcherFor(s.Store)
}

// Handles forwards HandlesFor to the wrapped store; see WorkStore.Handles.
func (s ResidentStore) Handles() StoreHandles { return forwardHandles(s, s.Store) }

func (s ResidentStore) unwrapClassLayer() Store { return s.Store }

var (
	_ ConditionalWritesResolveTargeter    = ResidentStore{}
	_ GuardedBatcherHandleProvider        = ResidentStore{}
	_ interface{ Handles() StoreHandles } = ResidentStore{}
)
