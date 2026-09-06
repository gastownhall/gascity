package beads

// ForeignIDCreator is an optional store capability for creating a bead whose
// explicit ID carries a prefix that differs from the store's own database prefix
// (a "foreign" prefix). The bd/Dolt store rejects a mismatched --id prefix unless
// forced; this capability performs the forced create so the class-store
// migration can copy a legacy graph bead into the graph store (id prefix gcg)
// while KEEPING its HQ/rig-era id (stable references must not be re-minted). The
// bead must carry a non-empty ID.
//
// The two stores that serve class bindings implement it — SQLiteStore and
// NativeDoltStore — because they are the two that fence a pinned id (invariant
// 16), and the migration copy is the one write that must cross a fence. A store
// with no prefix rules has nothing to exempt, so it does not implement this and
// callers reach it through the ordinary Create. Adding an implementation is
// therefore a decision about the fence, not a convenience: a caller type-asserts
// for this capability, and a store that gains it silently gains a write path
// nothing checks the namespace of.
type ForeignIDCreator interface {
	CreateWithForeignID(b Bead) (Bead, error)
}
