package beads

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
)

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
//
// # The residual, stated rather than implied (council A-F2, corrected by pr2 D-F3)
//
// The gate below is what keeps the linked library from applying a NUMBERED
// migration to a database bd owns: it compares the pair migrationSource.atLatest
// computes, so a database it admits is one whose migrate() returns at
// `current >= target` before it opens a migration file. It is not a read-only
// open, because beads exports none to an embedder at v1.3.0 — OpenBestAvailable
// goes to NewFromConfigWithOptions(ctx, beadsDir, nil), and Config.ReadOnly and
// Config.Gateway are both unreachable from outside internal/storage/dolt.
//
// An earlier version of this paragraph described what a writable open still
// does as "MigrateUp's idempotent, UNnumbered tail". That UNDERSTATED it (council
// pr2 D-F3), and the understatement is the part worth correcting, because the
// whole safety argument is read off this comment. At beads v1.3.0 a writable
// open of a database this gate ADMITS can still commit to it, on three paths:
//
//   - seedDoltIgnorePatterns runs BEFORE the migrationWorkNeeded short-circuit,
//     explicitly so an at-latest database cannot skip it (schema.go:603-645),
//     and on the no-work path commitSeededDoltIgnore commits the seed with
//     CALL DOLT_ADD('dolt_ignore') + CALL DOLT_COMMIT (schema.go:495-503, :656);
//   - healTrackedIgnoredCursorTable (ignored_cursor_untrack.go:134) runs there
//     too, "on every writable open" in its own words, untracks the legacy
//     cursor table and commits — and it is NOT one-shot, because a pull from a
//     not-yet-healed peer can re-introduce the shape;
//   - migrationWorkNeeded (schema.go:868-891) answers TRUE for a database at
//     both latest cursors when either cursor table lacks its content_hash
//     column or the custom statuses/types backfill is pending, so the migration
//     pass proper runs: it replays no numbered migration (migrate() still
//     returns at current >= target), but it ALTERs, backfills and commits
//     "schema: apply migrations" (schema.go:825-855).
//
// None of these is decided by the cursor pair or the sentinel reality this file
// checks. Those commits carry no --author, so they are attributed to the SQL
// session's identity rather than to the GIT_AUTHOR pair gc projects — which is
// also why nothing can tell gc's commit from another bd client's after the fact.
//
// ProxiedHeadUnmoved below is the belt-and-braces answer, and it is strictly
// wider than the gate: every one of those paths ends in a DOLT_COMMIT, and a
// commit moves HEAD. It DETECTS; it cannot prevent, because the write has
// happened by the time the open returns. Closing the hole rather than detecting
// it still needs a read-only open FROM BEADS: that is the standing ask,
// alongside SchemaVersions().
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
// runnable BEFORE the library open. The library's own open-time checks cover
// the MAIN lane only — CheckForwardDrift refuses it ahead, the shared-store
// migrate gate refuses it behind — and nothing in the open consults the ignored
// lane before migrating it (council pr2 D-F7), so this gate is the only check
// that lane gets.
//
// "Match" is equality, not "at least". A database BEHIND the library is one the
// library would migrate on open — a write to somebody else's shared database
// that gc never consented to — and one AHEAD is one the library would issue
// old-shape SQL against. Neither is gc's to fix through a store it opened to
// read.
//
// # Why the reality argument exists (council A-F2)
//
// The raw number in ignored_schema_migrations is not the number the library
// acts on. migrationSource.currentVersion clamps a cursor the live schema
// contradicts down to a "reality floor" — min(raw, 11) when
// `leases.granted_node` is missing, and 0 when `wisps`/`wisp_dependencies` are
// — and beads documents both shapes as real in the field. Comparing the RAW
// cursor therefore proved nothing: a database at raw ignored=26 with the
// sentinel absent passed this gate, the proxied open is WRITABLE
// (OpenBestAvailable -> NewFromConfigWithOptions(..., nil), and the library
// exports no read-only open to an embedder at v1.3.0), bd's own shared-store
// migrate gate consults the MAIN lane only and so returns nil, and MigrateUp
// then replayed ignored 0012-0025 against a database bd owns — from a handle gc
// opened purely to read.
//
// Gating on reality.EffectiveIgnored is what closes it, and it closes it by
// construction rather than by prediction: the effective pair is exactly what
// migrationSource.atLatest computes, so a database this function admits is one
// for which migrate() returns at `current >= target` before it opens a
// migration file. A clamped lane reads as ignored/behind, which is the true
// statement — the library believes that database is behind.
//
// Both lanes are checked and the main lane is reported first when both drift,
// because that is the one bd's own shared-store migration gate consults: a
// reader who sees "main" knows bd would have refused too, where "ignored" is
// drift only a reader comparing both lanes can see at all. The main lane needs
// no reality argument: mainSource declares no sentinels, so its raw cursor and
// its effective cursor are the same number.
//
// lane and dir are spelled with the ProxiedSkew* constants rather than a second
// private vocabulary, because the one consumer of a mismatch is the schema_skew
// verdict and two spellings of "ignored" is how a payload field and a matcher
// drift apart.
func CursorsMatchPinned(c proxyendpoint.Cursors, reality proxyendpoint.CursorReality) (ok bool, lane, dir string) {
	main, ignored := PinnedSchemaCursors()
	effectiveIgnored := reality.EffectiveIgnored(c.Ignored)
	switch {
	case c.Main > main:
		return false, ProxiedSkewLaneMain, ProxiedSkewDirAhead
	case c.Main < main:
		return false, ProxiedSkewLaneMain, ProxiedSkewDirBehind
	case effectiveIgnored > ignored:
		return false, ProxiedSkewLaneIgnored, ProxiedSkewDirAhead
	case effectiveIgnored < ignored:
		return false, ProxiedSkewLaneIgnored, ProxiedSkewDirBehind
	default:
		return true, "", ""
	}
}

// ProxiedHeadUnmoved reports whether gc's own library open left bd's database
// where it found it, and returns the head_moved verdict when it did not.
//
// # Why a HEAD hash and not another cursor read
//
// The gate above is a PRE-open check, and it is sound for what it covers: a
// database it admits is one whose numbered ignored series does not replay. It
// cannot cover the three commit paths in "The residual" above, because none of
// them is a function of the cursors. A post-open MAX(version) re-read would be
// no better — for an admitted database it is byte-identical by construction.
//
// A HEAD hash is the moving evidence for all of it: every one of those paths
// ends in a DOLT_COMMIT, and a healthy open moves nothing. The two halves cost
// no session of their own: the pre-open hash is a second column on the probe
// session's first statement (proxyendpoint's cursorExistsWithHeadQuery), and the
// re-read is one statement over the pool the library open itself just built
// (ProxiedOpenedHead).
//
// # Why it is not terminal, and what it therefore does NOT claim
//
// gc cannot tell a commit ITS open minted from one another bd client made in the
// same window: bd writes to this database constantly, and the schema commits
// carry no author gc could recognize. Demoting terminally on that would let
// another process's ordinary `bd update` permanently pin a scope to the bd front
// door. So the verdict is non-terminal: this open stands down loudly (the
// factory logs it with both hashes, see openProxiedNative), the opener forgets
// the memoized admission so the next open re-probes and is checked again, and a
// genuine per-open write shows up as a per-open demotion rather than as silence.
//
// The price of that choice is stated rather than hidden: on a city whose bd
// clients commit inside the probe-to-re-read window, an open that wrote nothing
// is demoted to the bd front door it would otherwise have replaced. That is the
// pre-PR2 path, so it costs forks, never correctness.
//
// # Both empty cases decline rather than agree
//
// An unobserved hash on either side is "not observed", never "unchanged" — and
// never "moved". A pin served from the admission memo carries no hash
// (Pin.withoutHead), because comparing against a value up to the memo's TTL old
// would make an unrelated write look like gc's; a re-read that produced nothing
// has nothing to compare. Neither is a verdict, and neither is agreement.
func ProxiedHeadUnmoved(pin Pin, observed string) error {
	before, after := strings.TrimSpace(pin.Head()), strings.TrimSpace(observed)
	if before == "" || after == "" || before == after {
		return nil
	}
	return NewProxiedVerdictError(ProxiedVerdictHeadMoved, fmt.Sprintf(
		"opening the linked library against database %q moved HEAD from %s to %s: "+
			"PR2's native lane serves reads only, so an open that commits may be gc writing to bd's database "+
			"(a writable library open can seed dolt_ignore, heal the tracked cursor table, or add a "+
			"content_hash column, each ending in a DOLT_COMMIT); this open takes bd's front door. "+
			"If another bd client committed in the same window this is that write and not gc's, "+
			"which is why the verdict is non-terminal and the next open re-probes",
		pin.Database(), before, after), nil)
}
