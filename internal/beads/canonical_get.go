package beads

// CanonicalGetter is an optional store capability for reading the row that
// mutations target when a backend can temporarily expose duplicate physical
// rows for one logical bead ID.
type CanonicalGetter interface {
	GetCanonical(id string) (Bead, error)
}

// GetCanonical reads the backend's canonical row when the store exposes that
// capability. Stores without duplicate-row semantics retain their ordinary Get
// behavior.
func GetCanonical(store Store, id string) (Bead, error) {
	if getter, ok := store.(CanonicalGetter); ok {
		return getter.GetCanonical(id)
	}
	return store.Get(id)
}
