package beads_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
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
