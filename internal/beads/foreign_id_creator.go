package beads

// ForeignIDCreator is an optional store capability for creating a bead whose
// explicit ID carries a prefix that differs from the store's own database prefix
// (a "foreign" prefix). The bd/Dolt store rejects a mismatched --id prefix unless
// forced; this capability performs the forced create so the class-store
// migration can copy a legacy graph bead into the graph store (id prefix gcg)
// while KEEPING its HQ/rig-era id (stable references must not be re-minted). The
// bead must carry a non-empty ID. Stores whose ids have no prefix rules (MemStore,
// FileStore) implement this by honoring the explicit id unconditionally.
type ForeignIDCreator interface {
	CreateWithForeignID(b Bead) (Bead, error)
}
