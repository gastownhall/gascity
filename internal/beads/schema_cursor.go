package beads

import (
	"fmt"
	"slices"
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
// ProxiedOpenUnmoved below is the belt-and-braces answer. It DETECTS; it cannot
// prevent, because the write has happened by the time the open returns. And it
// is NOT "strictly wider than the gate", which is what this paragraph used to
// claim (council pr2 E-S4). The argument was "every one of those paths ends in
// a DOLT_COMMIT, and a commit moves HEAD", and it fails on the dolt_ignore'd
// plane, because that plane is never committed:
//
//   - MigrateUp stages through stageSchemaTables and
//     existingCommittableTables, which filter on dolt_ignore
//     (schema.go:1060, :1098, dirtyTables(..., excludeIgnored=true) at :1159);
//     migrate() says the ignored source's "cursor and tables are never
//     committed to shared history" (:1737-1740); and the terminal DOLT_COMMIT
//     swallows "nothing to commit" (:851). So a pass whose only work is on the
//     ignored plane moves no HEAD.
//   - The third path above is one of them when the cursor table missing its
//     content_hash column is the IGNORED one: ensureContentHashColumn (:1272)
//     then ALTERs ignored_schema_migrations, a dolt_ignore pattern, and HEAD
//     does not move. (A database at ignored=26 lacks that column only after
//     out-of-band surgery — bootstrapSQL and the heal both carry it — but the
//     claim was "every one of those paths".)
//   - The ignored lane's REPLAY — 0012-0025 after the library clamps the
//     cursor, the hazard A-F2 and D-F7 are about — touches only ignored
//     tables (wisps, wisp_comments, leases, bd_events_journal/seq, events,
//     local_metadata) and records its cursor with INSERT IGNORE. It moves no
//     HEAD, and the cursor table does not change either.
//
// So the post-open observation reads the ignored plane as well, in the same
// statement as HEAD: the ignored cursor table's existence and the library's
// sentinel reality. What that half can and cannot conclude is stated on
// ProxiedOpenUnmoved, and the short version is that it is narrower than it
// sounds. Closing the hole rather than detecting some of it still needs a
// read-only open FROM BEADS: that is the standing ask, alongside
// SchemaVersions().
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
// `leases.granted_node` is missing, min(raw, 18) or min(raw, 21) when the
// dolt_ignored `events` or `bd_events_journal`/`bd_events_seq` tables are
// (beads v1.3.1), and 0 when `wisps`/`wisp_dependencies` are — and beads
// documents these shapes as real in the field. Comparing the RAW
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

// ProxiedOpenUnmoved reports whether gc's own library open left bd's database
// where it found it, and returns the head_moved verdict when it did not.
//
// # Why a HEAD hash and not another cursor read
//
// The gate above is a PRE-open check, and it is sound for what it covers: a
// database it admits is one whose numbered ignored series does not replay. It
// cannot cover the commit paths in "The residual" above, because none of them
// is a function of the cursors. A post-open MAX(version) re-read would be no
// better — for an admitted database it is byte-identical by construction.
//
// A HEAD hash is the moving evidence for every one of those paths that
// COMMITS, and a healthy open moves nothing. The two halves cost no session of
// their own: the pre-open hash is a second column on the probe session's first
// statement (proxyendpoint's cursorExistsWithHeadQuery), and the re-read is
// two statements on one pinned connection of the pool the library open itself
// just built (ProxiedOpenedObservation): the first advances a connection
// beads left on the pre-open session root (be-itm5, council pr2 E-S3), and the
// second is the observation.
//
// # The ignored plane, which HEAD cannot see (council pr2 E-S4)
//
// The same statement reads the ignored lane's cursor table and the library's
// sentinel reality, and the verdict fires when the EFFECTIVE ignored cursor
// they imply differs from the one admission found. An admitted pin's effective
// ignored cursor is its raw one — admission requires effective == pinned, and
// both floors sit below the pinned value — so the comparison is "is the plane
// still at the admitted cursor, as the linked library would compute it".
//
// What that sees: an open that returns with a sentinel absent or the cursor
// table gone. That is a database the NEXT writable open replays, and this leaf
// must not serve it.
//
// What it does NOT see, stated so nobody reads it as replay detection: a
// replay that completed. The library replays the ignored series only when it
// finds a sentinel missing, and the replay RESTORES what it found missing and
// records its cursor with INSERT IGNORE. The probe saw every sentinel present
// (it admitted), so after a completed replay everything this observation reads
// — the sentinels, the cursor table, HEAD — is what the probe saw. What the
// replay also rewrote (0015, for one, recomputes wisp is_blocked) is data on a
// plane with no history, which no cheap observation can attribute to this
// open. So no post-open comparison can tell
// "replayed and restored" from "untouched". This is the D-F7 residual's reach,
// not a new hole: detecting that write, rather than its leftovers, needs the
// read-only open from beads.
//
// # Whose move it was: the window
//
// bd writes to this database constantly, so a HEAD that moved between the
// probe and the re-read is usually another client's commit, and refusing every
// such open would demote a busy city's scopes to the bd front door for no
// reason. So a moved HEAD is attributed by the window between the two hashes
// (proxyendpoint.PostOpenWindow, read on the observation's own pinned
// connection), and it is let through only on positive evidence:
//
//   - A window that was not read, could not be read, holds more commits than
//     were read, or lists none is no evidence. It is head_moved, with the
//     reason in the detail (proxiedWindowUnattributable).
//   - A window carrying a library open's shape is head_moved naming the commit
//     or table that decided it (proxiedWindowOpenShape): a commit under the
//     open path's schema: label, a schema change on any table, or a change to
//     one of the tables only the open path writes. The table half is what
//     catches the open's uncommitted write swept into another client's -Am
//     commit, whose message says nothing.
//   - Anything else is another client's ordinary write. It is no verdict; the
//     memoized admission stands, and the ignored-plane check below still runs,
//     because a window of committed writes says nothing about the plane HEAD
//     cannot see.
//
// The verdict stays non-terminal. A window can be misread — a release that
// renames a schema: commit would hide it, which is what the pinned-module
// drift guard exists to catch — and a non-terminal stand-down re-probes on the
// next open, so a per-open write shows up as a per-open demotion rather than
// as a scope pinned to the bd front door for the process. Every site that
// meets the verdict logs it at WARN with both hashes (proxied_incident_log.go).
//
// # An unobserved HEAD declines rather than agrees
//
// An unobserved hash on either side is "not observed", never "unchanged" — and
// never "moved". A pin served from the admission memo carries no hash
// (Pin.withoutHead), because comparing against a value up to the memo's TTL old
// would make an unrelated write look like gc's; the caller skips the whole
// observation for such a pin. A report with no hash is not a report the
// production reader returns (ReadPostOpen fails instead), and it is declined
// here too.
func ProxiedOpenUnmoved(pin Pin, observed proxyendpoint.PostOpenReport) error {
	before, after := strings.TrimSpace(pin.Head()), strings.TrimSpace(observed.Head)
	if before == "" || after == "" {
		return nil
	}
	if before != after {
		if reason := proxiedWindowUnattributable(observed.Window); reason != "" {
			return NewProxiedVerdictError(ProxiedVerdictHeadMoved, fmt.Sprintf(
				"opening the linked library against database %q moved HEAD from %s to %s and the commits in between "+
					"cannot be attributed (%s): a move gc cannot show was another client's may be gc writing to "+
					"bd's database, so this open takes bd's front door and the next open re-probes",
				pin.Database(), before, after, reason), nil)
		}
		if shape := proxiedWindowOpenShape(observed.Window); shape != "" {
			return NewProxiedVerdictError(ProxiedVerdictHeadMoved, fmt.Sprintf(
				"opening the linked library against database %q moved HEAD from %s to %s through %s, which is the "+
					"shape of a library open's own write (a seed, heal, backfill, repair or migration commit): "+
					"PR2's native lane serves reads only, so this open takes bd's front door and the next open re-probes",
				pin.Database(), before, after, shape), nil)
		}
	}
	admitted := pin.Cursors().Ignored
	if now := observed.EffectiveIgnored(admitted); now != admitted {
		return NewProxiedVerdictError(ProxiedVerdictHeadMoved, fmt.Sprintf(
			"opening the linked library against database %q, with HEAD at %s, left the ignored plane's effective "+
				"cursor at %d where admission found %d (%s): the dolt_ignore'd plane is never committed, so HEAD "+
				"cannot see it, and a plane the next writable open would replay is not one this leaf may serve; "+
				"this open takes bd's front door and the next open re-probes",
			pin.Database(), after, now, admitted, observed.IgnoredPlaneReason()), nil)
	}
	return nil
}

// proxiedWindowUnattributable names why a moved HEAD's window is no evidence
// about who moved it, or returns "" for a window that can be attributed. A
// window that was not read, could not be read, holds more commits than were
// read, or lists no commits at all says nothing about the move, so it can
// never be read as another client's.
func proxiedWindowUnattributable(window *proxyendpoint.PostOpenWindow) string {
	switch {
	case window == nil:
		return "no window was read between the two hashes"
	case window.Unreadable != "":
		return "the window could not be read: " + window.Unreadable
	case window.Truncated:
		return fmt.Sprintf("the window holds more than %d commits", proxyendpoint.PostOpenWindowCommitLimit)
	case len(window.Commits) == 0:
		return "the window lists no commits"
	default:
		return ""
	}
}

// proxiedOpenCommitPrefix is the message prefix every commit the linked
// library's open path makes under its own label carries: the dolt_ignore seed,
// the migration commits and the #4356 untrack and sweep.
const proxiedOpenCommitPrefix = "schema:"

// proxiedOpenWriteTables are the tables the linked library's open path writes:
// its seed, heal, backfill and repair commits touch nothing else as data, and
// the repairs and migrations 0040/0041 commit dolt_nonlocal_tables under
// messages that carry no schema: prefix. A window that changes any of them is
// the open's shape whatever its commit messages say, because another client's
// -Am commit can sweep the open's uncommitted write into itself.
var proxiedOpenWriteTables = []string{
	"dolt_ignore",
	"schema_migrations",
	"ignored_schema_migrations",
	"custom_types",
	"custom_statuses",
	"dolt_nonlocal_tables",
}

// proxiedWindowOpenShape names the commit or table that makes window look
// like the linked library's own open-time write, or returns "" when nothing in
// it does. The checks are in order of how directly they name the open: a
// schema:-prefixed message, then a schema change on any table, then a change
// to a table only the open path writes.
func proxiedWindowOpenShape(window *proxyendpoint.PostOpenWindow) string {
	if window == nil {
		return ""
	}
	for _, message := range window.Commits {
		if strings.HasPrefix(strings.TrimSpace(message), proxiedOpenCommitPrefix) {
			return fmt.Sprintf("commit %q", message)
		}
	}
	for _, table := range window.Tables {
		if table.SchemaChange {
			return fmt.Sprintf("a schema change to table %q", table.Name)
		}
	}
	for _, table := range window.Tables {
		if slices.Contains(proxiedOpenWriteTables, table.Name) {
			return fmt.Sprintf("a change to table %q", table.Name)
		}
	}
	return ""
}
