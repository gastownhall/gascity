package beads

import "github.com/gastownhall/gascity/internal/beads/proxyendpoint"

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
// It returns two bare ints rather than a richer type because the two values are
// constants this package states on the library's behalf: there is nothing to
// name until beads exports SchemaVersions(), at which point the constants, this
// accessor and the drift test are all deleted together.
//
// An earlier version of this comment claimed that returning proxyendpoint.Cursors
// would close an import cycle (beads -> proxyendpoint -> internal/doltpool ->
// internal/config -> beads). That claim is FALSE and it misled a whole planning
// round into designing a port/adapter package to route around a cycle that does
// not exist. It was disproved twice: `go list -deps -test` over
// beads/proxyendpoint/doltpool/config shows no path back to internal/beads, and
// a `go build -overlay` injecting the import into this very file builds clean.
// CursorsMatchPinned below is that import, in production, standing as the
// executable form of the correction — so no future reader has to take the
// retraction on trust.
func PinnedSchemaCursors() (main, ignored int) {
	return SchemaCursorMain, SchemaCursorIgnored
}

// CursorsMatchPinned compares a probe's cursor pair against the pinned library's
// and, when they differ, names which lane drifted and in which direction.
//
// It takes proxyendpoint.Cursors — the type the probe actually produces — so the
// gate is one typed call at each of its call sites rather than two int
// comparisons a reader has to check for a swapped pair. The probe reads those
// numbers straight off disk with SELECT COALESCE(MAX(version),0) over its own
// connector, never through the linked library, which is what makes this gate
// runnable BEFORE the library open: beads' CheckForwardDrift runs at every open,
// so a mismatch discovered after the open would be the library's untyped error
// instead of gc's verdict.
//
// "Match" is equality, not "at least". A database BEHIND the library is one the
// library would migrate on open — a write to somebody else's shared database
// that gc never consented to — and one AHEAD is one the library would issue
// old-shape SQL against. Neither is gc's to fix through a store it opened to
// read.
//
// Both lanes are checked and the main lane is reported first when both drift,
// because that is the one bd's own shared-store migration gate consults: a
// reader who sees "main" knows bd would have refused too, where "ignored" is
// drift only a reader comparing both lanes can see at all.
//
// lane and dir are spelled with the ProxiedSkew* constants rather than a second
// private vocabulary, because the one consumer of a mismatch is the schema_skew
// verdict and two spellings of "ignored" is how a payload field and a matcher
// drift apart.
func CursorsMatchPinned(c proxyendpoint.Cursors) (ok bool, lane, dir string) {
	main, ignored := PinnedSchemaCursors()
	switch {
	case c.Main > main:
		return false, ProxiedSkewLaneMain, ProxiedSkewDirAhead
	case c.Main < main:
		return false, ProxiedSkewLaneMain, ProxiedSkewDirBehind
	case c.Ignored > ignored:
		return false, ProxiedSkewLaneIgnored, ProxiedSkewDirAhead
	case c.Ignored < ignored:
		return false, ProxiedSkewLaneIgnored, ProxiedSkewDirBehind
	default:
		return true, "", ""
	}
}
