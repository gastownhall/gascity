package beads

// ReadyLive returns ready beads using the backing store when a caching layer is
// present. Other Store implementations ignore the live-read intent and fall
// back to their normal Ready behavior.
func ReadyLive(store Store) ([]Bead, error) {
	type backingStore interface {
		Backing() Store
	}
	if cached, ok := store.(backingStore); ok && cached.Backing() != nil {
		return cached.Backing().Ready()
	}
	return store.Ready()
}
