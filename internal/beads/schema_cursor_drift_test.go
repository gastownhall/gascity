package beads_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
)

// The two migration directories beads embeds, and the suffix its own loader
// counts. Down migrations sit beside the up ones and are not versions the
// library would apply, so counting them would report a cursor no database ever
// reaches.
const (
	mainMigrationsDir    = "internal/storage/schema/migrations"
	ignoredMigrationsDir = "internal/storage/schema/migrations/ignored"
	migrationSuffix      = ".up.sql"
)

// TestSchemaCursorsMatchPinnedBeads is the whole reason the constants are
// allowed to exist.
//
// A constant that is only ever compared against itself proves nothing: gc would
// keep reporting "cursors equal, native open permitted" against a library that
// had moved on, and the first sign would be a migration the library ran on
// somebody's shared database. So the constants are compared against the pinned
// module's own migration directories, resolved out of the go module cache,
// which is the same source beads' schema.LatestVersion() reads from its
// embedded FS.
func TestSchemaCursorsMatchPinnedBeads(t *testing.T) {
	moduleDir := beadstest.PinnedBeadsModuleDir(t)
	version := beadstest.PinnedBeadsVersion(t)

	cases := []struct {
		lane string
		dir  string
		want int
	}{
		{lane: "main", dir: mainMigrationsDir, want: beads.SchemaCursorMain},
		{lane: "ignored", dir: ignoredMigrationsDir, want: beads.SchemaCursorIgnored},
	}

	for _, tc := range cases {
		t.Run(tc.lane, func(t *testing.T) {
			got := latestMigration(t, filepath.Join(moduleDir, filepath.FromSlash(tc.dir)))
			if got != tc.want {
				t.Fatalf("beads %s has %s lane at migration %d, but this build pins %d.\n"+
					"Update the constant in internal/beads/schema_cursor.go to %d and re-read the new "+
					"migrations: gc refuses a native open unless a database is at exactly these cursors, "+
					"so a stale pin either refuses every healthy scope or admits one the library would migrate.",
					version, tc.lane, got, tc.want, got)
			}
		})
	}
}

// TestIgnoredSentinelsMatchPinnedBeads is the sentinel half of the cursor drift
// pin.
//
// The floors and the sentinel names are the LIBRARY's, restated here because
// beads exports none of them. A constant only ever compared against itself
// proves nothing, so — exactly as TestSchemaCursorsMatchPinnedBeads does for
// the two cursors — they are compared against the pinned module's own source in
// the go module cache. A beads release that renumbers the replay floor or
// renames a sentinel breaks this test rather than silently returning gc's gate
// to the shape that let a read path migrate somebody else's database.
func TestIgnoredSentinelsMatchPinnedBeads(t *testing.T) {
	source := filepath.Join(beadstest.PinnedBeadsModuleDir(t),
		filepath.FromSlash("internal/storage/schema/schema.go"))
	body, err := os.ReadFile(source) //nolint:gosec // a path derived from the module cache
	if err != nil {
		t.Fatalf("read the pinned library's schema source %s: %v", source, err)
	}
	text := string(body)

	for _, table := range proxyendpoint.IgnoredSentinelTables() {
		if !strings.Contains(text, strconv.Quote(table)) {
			t.Errorf("the pinned library no longer names the ignored-lane sentinel table %q; "+
				"re-read ignoredSource in %s and update proxyendpoint.IgnoredSentinelTables()", table, source)
		}
	}
	want := "{table: " + strconv.Quote(proxyendpoint.IgnoredSentinelColumnTable) +
		", column: " + strconv.Quote(proxyendpoint.IgnoredSentinelColumnName) +
		", replayFloor: " + strconv.Itoa(proxyendpoint.IgnoredSentinelColumnFloor) + "}"
	if !strings.Contains(text, want) {
		t.Fatalf("the pinned library's ignoredSource no longer declares %s.\n"+
			"gc's proxied schema gate computes the library's own effective ignored cursor from these "+
			"values; a stale copy either refuses every healthy scope or admits one the library would migrate.\n"+
			"Re-read ignoredSource in %s.", want, source)
	}
}

// TestPinnedSchemaCursorsProjectsBothConstants pins the accessor's order as well
// as its values: it returns two bare ints, and a caller that swapped them would
// compare the ignored lane against the main constant and read as healthy.
func TestPinnedSchemaCursorsProjectsBothConstants(t *testing.T) {
	main, ignored := beads.PinnedSchemaCursors()
	if main != beads.SchemaCursorMain {
		t.Errorf("PinnedSchemaCursors() main = %d, want %d", main, beads.SchemaCursorMain)
	}
	if ignored != beads.SchemaCursorIgnored {
		t.Errorf("PinnedSchemaCursors() ignored = %d, want %d", ignored, beads.SchemaCursorIgnored)
	}
	if main == ignored {
		t.Fatal("the two lanes are at the same version, so this test cannot detect a swapped pair; assert the values directly instead")
	}
}

// TestCursorsMatchPinnedReportsLaneAndDirection pins the typed gate the proxied
// admission path runs before it opens the linked library against somebody
// else's database.
//
// It asserts the direction's ORIENTATION as much as its value: dir describes
// where the database sits relative to this binary, so a database one migration
// up on the main lane is "ahead". Getting that backwards would print a verdict
// telling an operator to upgrade the thing that is already newer.
//
// That this test imports proxyendpoint at all is the point of the item: the
// retired comment on PinnedSchemaCursors claimed the import closed a cycle, and
// a compiling test that passes proxyendpoint.Cursors into internal/beads is the
// executable retraction.
func TestCursorsMatchPinnedReportsLaneAndDirection(t *testing.T) {
	main, ignored := beads.PinnedSchemaCursors()

	cases := []struct {
		name     string
		cursors  proxyendpoint.Cursors
		reality  proxyendpoint.CursorReality
		wantOK   bool
		wantLane string
		wantDir  string
	}{
		{
			name:    "equal",
			cursors: proxyendpoint.Cursors{Main: main, Ignored: ignored},
			wantOK:  true,
		},
		{
			name:     "main ahead",
			cursors:  proxyendpoint.Cursors{Main: main + 1, Ignored: ignored},
			wantLane: beads.ProxiedSkewLaneMain,
			wantDir:  beads.ProxiedSkewDirAhead,
		},
		{
			name:     "main behind",
			cursors:  proxyendpoint.Cursors{Main: main - 1, Ignored: ignored},
			wantLane: beads.ProxiedSkewLaneMain,
			wantDir:  beads.ProxiedSkewDirBehind,
		},
		{
			name:     "ignored ahead",
			cursors:  proxyendpoint.Cursors{Main: main, Ignored: ignored + 1},
			wantLane: beads.ProxiedSkewLaneIgnored,
			wantDir:  beads.ProxiedSkewDirAhead,
		},
		{
			name:     "ignored behind",
			cursors:  proxyendpoint.Cursors{Main: main, Ignored: ignored - 1},
			wantLane: beads.ProxiedSkewLaneIgnored,
			wantDir:  beads.ProxiedSkewDirBehind,
		},
		{
			// Both lanes drifted: the main lane is reported, because that is
			// the one bd's own migration gate consults.
			name:     "both drift reports main",
			cursors:  proxyendpoint.Cursors{Main: main - 1, Ignored: ignored - 1},
			wantLane: beads.ProxiedSkewLaneMain,
			wantDir:  beads.ProxiedSkewDirBehind,
		},
		{
			// A probe against a database with no migration rows reads zeros.
			// That must NOT read as "equal" through some zero-value shortcut.
			name:     "unmigrated database",
			cursors:  proxyendpoint.Cursors{},
			wantLane: beads.ProxiedSkewLaneMain,
			wantDir:  beads.ProxiedSkewDirBehind,
		},
		{
			// Council A-F2. The cursor ON DISK is equal on both lanes, and this
			// gate used to pass it. The linked library does not read that
			// number: with `leases.granted_node` absent it computes
			// min(26, 11) = 11, decides the ignored lane is behind, and
			// MigrateUp replays ignored 0012-0025 against a database bd owns —
			// from a handle gc opened purely to read. A clamped lane is behind,
			// and the gate must say so BEFORE the open.
			name:     "a clamped ignored lane is behind however the cursor reads",
			cursors:  proxyendpoint.Cursors{Main: main, Ignored: ignored},
			reality:  proxyendpoint.CursorReality{Limited: true, Floor: 11, Missing: "leases.granted_node"},
			wantLane: beads.ProxiedSkewLaneIgnored,
			wantDir:  beads.ProxiedSkewDirBehind,
		},
		{
			// A missing sentinel TABLE floors the lane at zero, which is the
			// harsher half of the same shape.
			name:     "a missing sentinel table floors the ignored lane at zero",
			cursors:  proxyendpoint.Cursors{Main: main, Ignored: ignored},
			reality:  proxyendpoint.CursorReality{Limited: true, Floor: 0, Missing: "wisp_dependencies"},
			wantLane: beads.ProxiedSkewLaneIgnored,
			wantDir:  beads.ProxiedSkewDirBehind,
		},
		{
			// A floor at or above the cursor changes nothing: the library
			// believes the cursor as read, and so does the gate.
			name:    "a floor above the cursor is not a clamp",
			cursors: proxyendpoint.Cursors{Main: main, Ignored: ignored},
			reality: proxyendpoint.CursorReality{Limited: true, Floor: ignored, Missing: "leases.granted_node"},
			wantOK:  true,
		},
		{
			// The main lane drifting still outranks a clamped ignored lane, so
			// the operator is told the thing bd's own gate would also refuse.
			name:     "both drift reports main even when the ignored lane is clamped",
			cursors:  proxyendpoint.Cursors{Main: main - 1, Ignored: ignored},
			reality:  proxyendpoint.CursorReality{Limited: true, Floor: 11, Missing: "leases.granted_node"},
			wantLane: beads.ProxiedSkewLaneMain,
			wantDir:  beads.ProxiedSkewDirBehind,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, lane, dir := beads.CursorsMatchPinned(tc.cursors, tc.reality)
			if ok != tc.wantOK {
				t.Fatalf("CursorsMatchPinned(%v, %+v) ok = %v, want %v", tc.cursors, tc.reality, ok, tc.wantOK)
			}
			if lane != tc.wantLane || dir != tc.wantDir {
				t.Errorf("CursorsMatchPinned(%v, %+v) = lane %q dir %q, want lane %q dir %q",
					tc.cursors, tc.reality, lane, dir, tc.wantLane, tc.wantDir)
			}
		})
	}
}

// latestMigration returns the highest migration version in dir, counting the
// same files beads' own migrationSource.list() counts: `.up.sql` entries at the
// top level of the directory, versioned by the integer before the first
// underscore. The ignored lane is a SUBdirectory of the main one, so the main
// lane's scan must not recurse — a walk would fold the ignored versions into
// the main cursor and both would read as the same number.
func latestMigration(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations directory %s: %v", dir, err)
	}
	latest, counted := 0, 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), migrationSuffix) {
			continue
		}
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			t.Fatalf("migration %q in %s has no version prefix", entry.Name(), dir)
		}
		version, err := strconv.Atoi(prefix)
		if err != nil {
			t.Fatalf("migration %q in %s has an unparseable version prefix: %v", entry.Name(), dir, err)
		}
		counted++
		if version > latest {
			latest = version
		}
	}
	if counted == 0 {
		t.Fatalf("no %s migrations under %s; the pin would silently read as 0", migrationSuffix, dir)
	}
	return latest
}

// TestNativeGetFilterShapeMatchesTheH1Claim is council B-F6, made executable.
//
// The wrapper's H1 register and its Get doc both asserted that the native Get
// "does not set IncludeEphemeral, so a wisp-tier bead is invisible to it". Two
// things are wrong with that at beads v1.3.0, and the second one is load-bearing
// twice over — it is cited as the justification for the bd fallback AND used to
// argue H1 is a regression on this lane:
//
//   - IncludeEphemeral is not a field of types.IssueFilter. It belongs to
//     types.WorkFilter, GetReadyWork's filter.
//   - issueops.searchInTx routes on Ephemeral/SkipWisps, and with Ephemeral nil
//     and SkipWisps unset it MERGES the wisps table.
//
// A comment cannot be compiled, so this asserts the two facts the corrected
// comment rests on, against the pinned module's own source. A beads release
// that moves IncludeEphemeral onto IssueFilter, or that stops merging the wisps
// plane for a nil-Ephemeral filter, breaks this test rather than silently
// making the old claim true again by accident.
func TestNativeGetFilterShapeMatchesTheH1Claim(t *testing.T) {
	moduleDir := beadstest.PinnedBeadsModuleDir(t)

	types, err := os.ReadFile(filepath.Join(moduleDir, filepath.FromSlash("internal/types/types.go")))
	if err != nil {
		t.Fatalf("read the pinned library's types: %v", err)
	}
	if !regexp.MustCompile(`(?m)^\s*IncludeEphemeral\s+bool`).Match(types) {
		t.Fatal("the pinned library no longer declares IncludeEphemeral at all; re-read the H1 note " +
			"on ProxiedStore before trusting it")
	}
	// The field lives on WorkFilter, not IssueFilter. Asserted by position: the
	// declaration must fall inside the WorkFilter struct.
	if !declaredInStruct(string(types), "WorkFilter", "IncludeEphemeral") {
		t.Error("IncludeEphemeral is no longer a WorkFilter field; the H1 note on ProxiedStore says it is")
	}
	if declaredInStruct(string(types), "IssueFilter", "IncludeEphemeral") {
		t.Error("IncludeEphemeral is now an IssueFilter field, so the native Get COULD set it; " +
			"the H1 note on ProxiedStore is written on the opposite assumption")
	}

	search, err := os.ReadFile(filepath.Join(moduleDir,
		filepath.FromSlash("internal/storage/issueops/search.go")))
	if err != nil {
		t.Fatalf("read the pinned library's search: %v", err)
	}
	if !strings.Contains(string(search), "if filter.Ephemeral == nil || !*filter.Ephemeral {") {
		t.Fatal("searchInTx no longer merges the wisps table for a nil-Ephemeral filter; the native " +
			"Get's visibility of the wisps plane is the fact H1's corrected note rests on")
	}
}

// declaredInStruct reports whether field is declared inside the named struct
// type. It scans by brace balance from the type declaration, which is enough
// for a flat Go struct and does not need a parser for a file this test only
// reads.
func declaredInStruct(source, structName, field string) bool {
	start := strings.Index(source, "type "+structName+" struct {")
	if start < 0 {
		return false
	}
	depth := 0
	for i := start; i < len(source); i++ {
		switch source[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				body := source[start:i]
				return regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(field) + `\s`).MatchString(body)
			}
		}
	}
	return false
}
