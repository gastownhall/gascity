package beads

// The typed class handles forward HandlesFor (typed-stores design §3.3.2,
// T7a): HandlesFor(h) is the store's own handles, so a handle over a
// CachingStore answers the cache's cached and live readers rather than the
// logical wrappers over the handle, whose Live.Get would serve the cache. The
// writer is the handle, so a write through the handles still goes through
// it.

var (
	_ interface{ Handles() StoreHandles } = WorkStore{}
	_ interface{ Handles() StoreHandles } = GraphStore{}
	_ interface{ Handles() StoreHandles } = SessionStore{}
	_ interface{ Handles() StoreHandles } = MailStore{}
	_ interface{ Handles() StoreHandles } = OrdersStore{}
	_ interface{ Handles() StoreHandles } = NudgesStore{}
	_ interface{ Handles() StoreHandles } = MoleculeTarget{}
)

// HasExplicitHandles reports whether the store below s's class layers has
// handles of its own (a CachingStore, a policy store): whether HandlesFor(s)
// answers a real cached and live tier, rather than the logical wrappers over
// one store. A class layer forwards Handles whatever lies beneath it, so
// asserting the Handles method on s itself would say yes for a handle over a
// plain store, and a caller that pays a second, live read only for explicit
// handles would pay it there too.
func HasExplicitHandles(s StoreReader) bool {
	_, ok := physicalStore(s).(interface{ Handles() StoreHandles })
	return ok
}

// forwardHandles is HandlesFor(inner) with layer as the writer.
func forwardHandles(layer, inner Store) StoreHandles {
	handles := HandlesFor(inner)
	handles.Writer = layer
	return handles
}

// Handles forwards HandlesFor to the wrapped store, with the handle as the
// writer.
func (s WorkStore) Handles() StoreHandles { return forwardHandles(s, s.Store) }

// Handles forwards HandlesFor to the wrapped store; see WorkStore.Handles.
func (s GraphStore) Handles() StoreHandles { return forwardHandles(s, s.Store) }

// Handles forwards HandlesFor to the wrapped store; see WorkStore.Handles.
func (s SessionStore) Handles() StoreHandles { return forwardHandles(s, s.Store) }

// Handles forwards HandlesFor to the wrapped store; see WorkStore.Handles.
func (s MailStore) Handles() StoreHandles { return forwardHandles(s, s.Store) }

// Handles forwards HandlesFor to the wrapped store; see WorkStore.Handles.
func (s OrdersStore) Handles() StoreHandles { return forwardHandles(s, s.Store) }

// Handles forwards HandlesFor to the wrapped store; see WorkStore.Handles.
func (s NudgesStore) Handles() StoreHandles { return forwardHandles(s, s.Store) }

// Handles forwards HandlesFor to the wrapped store; see WorkStore.Handles.
func (t MoleculeTarget) Handles() StoreHandles { return forwardHandles(t, t.Store) }
