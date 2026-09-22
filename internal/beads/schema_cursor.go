package beads

// The schema cursors of the beads library this binary is linked against: the
// highest migration in each of bd's two lanes at the pinned version.
//
// They exist because beads exports no accessor for them.
// schema.LatestVersion() and schema.LatestIgnoredVersion() are internal to
// beads, so an embedder that needs to know whether a shared database is at the
// same schema as its own linked library has to state the numbers and keep them
// honest. SchemaCursorsMatchPinnedBeads does that: it reads the pinned module's
// migration directories out of the go module cache and fails when either
// constant drifts, so a beads bump cannot quietly move the schema out from
// under a comparison that still reads as true.
//
// Both lanes are pinned, not just the main one. bd's own shared-store migration
// gate consults the main lane alone, and MigrateUp then applies pending
// IGNORED-lane migrations without asking anyone — so a library one ahead on the
// ignored lane would migrate a shared database on open. A reader comparing only
// the main cursor would never see it coming.
//
// They are deleted when beads exports SchemaVersions(), which is the standing
// ask; the drift test goes with them.
const (
	// SchemaCursorMain is schema.LatestVersion() for the pinned library.
	SchemaCursorMain = 66
	// SchemaCursorIgnored is schema.LatestIgnoredVersion() for the pinned
	// library.
	SchemaCursorIgnored = 26
)

// PinnedSchemaCursors returns the pair a proxied database must already be at
// before gc may open the linked library against it.
//
// "Already at", exactly — not "at least". A database BEHIND the library is one
// the library would migrate on open, and a database AHEAD is one the library
// would issue old-shape SQL against; neither is gc's to fix through a store it
// opened for a read.
//
// It returns two ints rather than the proxyendpoint.Cursors the probe produces,
// because importing that package from here closes a cycle: proxyendpoint dials
// through internal/doltpool, whose own tests reach internal/config and back
// round to internal/beads. The comparison is one line at the two call sites that
// need it, and it is not worth a package boundary to save.
func PinnedSchemaCursors() (main, ignored int) {
	return SchemaCursorMain, SchemaCursorIgnored
}
