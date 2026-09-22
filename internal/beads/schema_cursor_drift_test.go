package beads_test

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
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
// pin, and it is STRUCTURAL over the library's sentinel set (council pr2 D-F4).
//
// The floors and the sentinel names are the LIBRARY's, restated here because
// beads exports none of them. The first version of this pin asked whether each
// name gc knows still appeared somewhere in schema.go. That is one-directional:
// a beads release that ADDS a sentinel table or column — the direction that
// reopens A-F2, because gc's probe would not read it and would admit a database
// the library clamps — left every known name present and the test green. It was
// also blind to a removal: "wisps" appears in doltIgnorePatterns as well as in
// sentinelTables, so deleting it from the sentinel list changed nothing the old
// substring check could see.
//
// So the pin parses the pinned schema package — every non-test file, because a
// sentinel declared in a sibling file is just as live — and compares the WHOLE
// declared set against gc's:
//
//   - the only migrationSource literal that declares any sentinel is
//     ignoredSource (a main-lane sentinel is one gc's probe never reads);
//   - ignoredSource.sentinelTables equals gc's list, in ORDER (the library
//     probes tables first and short-circuits on the first absent one);
//   - ignoredSource.sentinelColumns is exactly the one column gc reads, with
//     the same replay floor;
//   - cursorRealityFloor's missing-table arm floors at gc's table floor.
func TestIgnoredSentinelsMatchPinnedBeads(t *testing.T) {
	dir := filepath.Join(beadstest.PinnedBeadsModuleDir(t), filepath.FromSlash("internal/storage/schema"))
	sources := readGoSources(t, dir)

	declared, err := declaredSentinels(sources)
	if err != nil {
		t.Fatalf("read the pinned library's sentinel declarations under %s: %v", dir, err)
	}
	if _, ok := declared["mainSource"]; !ok {
		t.Fatalf("found no mainSource literal under %s; the pin is broken, not satisfied (saw %v)", dir, declared)
	}
	for _, problem := range sentinelDrift(declared) {
		t.Errorf("%s.\ngc's proxied schema gate computes the library's own effective ignored cursor from "+
			"proxyendpoint's sentinel list; a stale copy either refuses every healthy scope or admits one "+
			"the library would migrate. Re-read the migrationSource literals under %s.", problem, dir)
	}

	floor, err := sentinelTableFloor(sources)
	if err != nil {
		t.Fatalf("read cursorRealityFloor's missing-table arm under %s: %v", dir, err)
	}
	if floor != proxyendpoint.IgnoredSentinelTableFloor {
		t.Errorf("the pinned library floors a missing sentinel table at %d, gc at %d", floor, proxyendpoint.IgnoredSentinelTableFloor)
	}
}

// TestSentinelDriftPinSeesEveryDirection proves the pin above is structural by
// feeding it the library shapes the old substring pin could not see, and one it
// must accept. Each row is a synthetic schema package; the comparison is the
// same function the real pin runs.
func TestSentinelDriftPinSeesEveryDirection(t *testing.T) {
	const header = "package schema\n\ntype schemaSentinelColumn struct{ table, column string; replayFloor int }\n" +
		"type migrationSource struct{ cursorTable string; sentinelTables []string; sentinelColumns []schemaSentinelColumn }\n"
	const pinned = `var (
	mainSource    = migrationSource{cursorTable: "schema_migrations"}
	ignoredSource = migrationSource{
		cursorTable:     "ignored_schema_migrations",
		sentinelTables:  []string{"wisps", "wisp_dependencies"},
		sentinelColumns: []schemaSentinelColumn{{table: "leases", column: "granted_node", replayFloor: 11}},
	}
)
var doltIgnorePatterns = []string{"wisps", "wisp_dependencies"}
`
	cases := []struct {
		name    string
		sources map[string]string
		want    string // "" means no drift
	}{
		{name: "the pinned shape", sources: map[string]string{"schema.go": header + pinned}},
		{
			name: "an ADDED sentinel table",
			sources: map[string]string{"schema.go": header + strings.Replace(pinned,
				`[]string{"wisps", "wisp_dependencies"},`, `[]string{"wisps", "wisp_dependencies", "wisp_events"},`, 1)},
			want: "sentinelTables",
		},
		{
			name: "an ADDED sentinel column",
			sources: map[string]string{"schema.go": header + strings.Replace(pinned,
				`replayFloor: 11}},`, `replayFloor: 11}, {table: "wisps", column: "lease_id", replayFloor: 20}},`, 1)},
			want: "sentinelColumns",
		},
		{
			// "wisps" still appears in doltIgnorePatterns, which is the
			// substring the old pin matched.
			name: "a REMOVED sentinel table whose name survives elsewhere",
			sources: map[string]string{"schema.go": header + strings.Replace(pinned,
				`[]string{"wisps", "wisp_dependencies"},`, `[]string{"wisp_dependencies"},`, 1)},
			want: "sentinelTables",
		},
		{
			name: "reordered sentinel tables",
			sources: map[string]string{"schema.go": header + strings.Replace(pinned,
				`[]string{"wisps", "wisp_dependencies"},`, `[]string{"wisp_dependencies", "wisps"},`, 1)},
			want: "sentinelTables",
		},
		{
			name: "a MAIN-lane sentinel",
			sources: map[string]string{"schema.go": header + strings.Replace(pinned,
				`mainSource    = migrationSource{cursorTable: "schema_migrations"}`,
				`mainSource    = migrationSource{cursorTable: "schema_migrations", sentinelTables: []string{"issues"}}`, 1)},
			want: "mainSource",
		},
		{
			name: "a sentinel declared in a sibling file",
			sources: map[string]string{
				"schema.go":  header + pinned,
				"sibling.go": "package schema\n\nvar thirdSource = migrationSource{sentinelTables: []string{\"x\"}}\n",
			},
			want: "thirdSource",
		},
		{
			name: "a moved replay floor",
			sources: map[string]string{"schema.go": header + strings.Replace(pinned,
				`replayFloor: 11}`, `replayFloor: 12}`, 1)},
			want: "sentinelColumns",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sources := map[string][]byte{}
			for name, body := range tc.sources {
				sources[name] = []byte(body)
			}
			declared, err := declaredSentinels(sources)
			if err != nil {
				t.Fatalf("declaredSentinels: %v", err)
			}
			problems := strings.Join(sentinelDrift(declared), "\n")
			if tc.want == "" {
				if problems != "" {
					t.Fatalf("the pinned shape reported drift:\n%s", problems)
				}
				return
			}
			if !strings.Contains(problems, tc.want) {
				t.Fatalf("the pin did not see this drift (want a problem naming %q), got:\n%s", tc.want, problems)
			}
		})
	}
}

// sentinelColumnDecl is one schemaSentinelColumn literal.
type sentinelColumnDecl struct {
	table, column string
	floor         int
}

// sentinelDecl is what one migrationSource literal declares about sentinels.
type sentinelDecl struct {
	tables  []string
	columns []sentinelColumnDecl
}

func (d sentinelDecl) empty() bool { return len(d.tables) == 0 && len(d.columns) == 0 }

// readGoSources reads every non-test Go file directly under dir.
func readGoSources(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	sources := map[string][]byte{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a path derived from the module cache
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		sources[name] = body
	}
	if len(sources) == 0 {
		t.Fatalf("no Go sources under %s; the pin would read as satisfied", dir)
	}
	return sources
}

// declaredSentinels returns, for every package-level var initialized with a
// migrationSource composite literal, the sentinels that literal declares.
//
// A sentinel field whose value is anything other than a literal gc can read is
// an error, not an omission: a pin that skipped what it could not parse would be
// the one-directional pin again.
func declaredSentinels(sources map[string][]byte) (map[string]sentinelDecl, error) {
	fileSet := token.NewFileSet()
	declared := map[string]sentinelDecl{}
	for name, body := range sources {
		parsed, err := parser.ParseFile(fileSet, name, body, 0)
		if err != nil {
			return nil, err
		}
		for _, decl := range parsed.Decls {
			general, ok := decl.(*ast.GenDecl)
			if !ok || general.Tok != token.VAR {
				continue
			}
			for _, spec := range general.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, ident := range value.Names {
					if i >= len(value.Values) {
						continue
					}
					literal, ok := value.Values[i].(*ast.CompositeLit)
					if !ok || !isIdent(literal.Type, "migrationSource") {
						continue
					}
					d, err := sentinelsOf(literal)
					if err != nil {
						return nil, fmt.Errorf("%s: %s: %w", name, ident.Name, err)
					}
					declared[ident.Name] = d
				}
			}
		}
	}
	return declared, nil
}

// sentinelsOf reads the sentinel fields of one migrationSource literal.
func sentinelsOf(literal *ast.CompositeLit) (sentinelDecl, error) {
	var d sentinelDecl
	for _, element := range literal.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			return d, errors.New("a positional migrationSource literal; this pin reads keyed fields only")
		}
		switch keyName(field.Key) {
		case "sentinelTables":
			list, ok := field.Value.(*ast.CompositeLit)
			if !ok {
				return d, errors.New("sentinelTables is not a literal")
			}
			for _, item := range list.Elts {
				table, err := stringLit(item)
				if err != nil {
					return d, fmt.Errorf("sentinelTables: %w", err)
				}
				d.tables = append(d.tables, table)
			}
		case "sentinelColumns":
			list, ok := field.Value.(*ast.CompositeLit)
			if !ok {
				return d, errors.New("sentinelColumns is not a literal")
			}
			for _, item := range list.Elts {
				column, ok := item.(*ast.CompositeLit)
				if !ok {
					return d, errors.New("a sentinelColumns entry is not a literal")
				}
				var c sentinelColumnDecl
				for _, part := range column.Elts {
					kv, ok := part.(*ast.KeyValueExpr)
					if !ok {
						return d, errors.New("a positional schemaSentinelColumn; this pin reads keyed fields only")
					}
					var err error
					switch keyName(kv.Key) {
					case "table":
						c.table, err = stringLit(kv.Value)
					case "column":
						c.column, err = stringLit(kv.Value)
					case "replayFloor":
						c.floor, err = intLit(kv.Value)
					default:
						err = fmt.Errorf("unknown schemaSentinelColumn field %s", keyName(kv.Key))
					}
					if err != nil {
						return d, fmt.Errorf("sentinelColumns: %w", err)
					}
				}
				d.columns = append(d.columns, c)
			}
		}
	}
	return d, nil
}

// sentinelDrift compares the library's declared sentinel set with gc's and
// returns one problem per disagreement.
func sentinelDrift(declared map[string]sentinelDecl) []string {
	var problems []string
	ignored, ok := declared["ignoredSource"]
	if !ok {
		problems = append(problems, "the library declares no ignoredSource literal")
	}
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name != "ignoredSource" && !declared[name].empty() {
			problems = append(problems, fmt.Sprintf("%s declares sentinels %+v that gc's probe never reads", name, declared[name]))
		}
	}
	if !ok {
		return problems
	}
	if want := proxyendpoint.IgnoredSentinelTables(); !slices.Equal(ignored.tables, want) {
		problems = append(problems, fmt.Sprintf("ignoredSource.sentinelTables is %q, gc probes %q (order matters: the library short-circuits on the first absent table)", ignored.tables, want))
	}
	want := []sentinelColumnDecl{{
		table:  proxyendpoint.IgnoredSentinelColumnTable,
		column: proxyendpoint.IgnoredSentinelColumnName,
		floor:  proxyendpoint.IgnoredSentinelColumnFloor,
	}}
	if !slices.Equal(ignored.columns, want) {
		problems = append(problems, fmt.Sprintf("ignoredSource.sentinelColumns is %+v, gc probes exactly %+v", ignored.columns, want))
	}
	return problems
}

// sentinelTableFloor reads the value cursorRealityFloor returns for a missing
// sentinel TABLE: the first result of the `return <n>, true, nil` inside its
// range over m.sentinelTables.
func sentinelTableFloor(sources map[string][]byte) (int, error) {
	fileSet := token.NewFileSet()
	for name, body := range sources {
		parsed, err := parser.ParseFile(fileSet, name, body, 0)
		if err != nil {
			return 0, err
		}
		for _, decl := range parsed.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Name.Name != "cursorRealityFloor" || function.Body == nil {
				continue
			}
			floor, found := 0, false
			var walkErr error
			ast.Inspect(function.Body, func(node ast.Node) bool {
				loop, ok := node.(*ast.RangeStmt)
				if !ok || found {
					return !found
				}
				if selector, ok := loop.X.(*ast.SelectorExpr); !ok || selector.Sel.Name != "sentinelTables" {
					return true
				}
				ast.Inspect(loop.Body, func(inner ast.Node) bool {
					ret, ok := inner.(*ast.ReturnStmt)
					if !ok || found || len(ret.Results) != 3 || !isIdent(ret.Results[1], "true") {
						return !found
					}
					floor, walkErr = intLit(ret.Results[0])
					found = true
					return false
				})
				return false
			})
			if walkErr != nil {
				return 0, walkErr
			}
			if !found {
				return 0, errors.New("cursorRealityFloor has no `return <floor>, true, nil` inside its range over sentinelTables")
			}
			return floor, nil
		}
	}
	return 0, errors.New("no cursorRealityFloor function")
}

func isIdent(expr ast.Expr, name string) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == name
}

func keyName(expr ast.Expr) string {
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

func stringLit(expr ast.Expr) (string, error) {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", errors.New("not a string literal")
	}
	return strconv.Unquote(literal.Value)
}

func intLit(expr ast.Expr) (int, error) {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.INT {
		return 0, errors.New("not an integer literal")
	}
	return strconv.Atoi(literal.Value)
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
