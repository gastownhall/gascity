package beads_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
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
//
// And, since council pr2 E-S5 / E-I3, the shapes that version still skipped:
//
//   - every migrationSource literal is read wherever it sits — by pointer, as
//     an element of a slice, array or map of sources, or inside a function —
//     not only `var x = migrationSource{…}`;
//   - a sentinel field other than the two gc reads is an error, and
//     migrationSource's field set is pinned, so a new sentinel KIND cannot
//     arrive unread;
//   - a reference to a sentinel field outside cursorRealityFloor (an init()
//     append, a reassignment) is a problem, because it changes the set without
//     changing the literal;
//   - cursorRealityFloor itself is pinned by a digest of its tokens, so a new
//     clamp arm fails the pin even when every literal is unchanged.
func TestIgnoredSentinelsMatchPinnedBeads(t *testing.T) {
	dir := filepath.Join(beadstest.PinnedBeadsModuleDir(t), filepath.FromSlash("internal/storage/schema"))
	sources := readGoSources(t, dir)

	declared, problems := readSentinelPin(sources)
	if _, ok := declared["mainSource"]; !ok {
		t.Fatalf("found no mainSource literal under %s; the pin is broken, not satisfied (saw %v, problems %q)", dir, declared, problems)
	}
	for _, problem := range problems {
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

	parsed, err := parseSources(sources)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	digest, err := cursorRealityFloorDigest(parsed)
	if err != nil {
		t.Fatalf("digest cursorRealityFloor under %s: %v", dir, err)
	}
	if digest != pinnedCursorRealityFloorDigest {
		t.Errorf("cursorRealityFloor changed at the pinned beads (token digest %s, pinned %s).\n"+
			"A new clamp arm or a read of a new field changes which ignored cursor the library acts on, and "+
			"proxyendpoint's reality derivation (readIgnoredReality, ReadPostOpen) mirrors this function by hand. "+
			"Re-read it under %s, bring the mirror and the sentinel constants into line, then update "+
			"pinnedCursorRealityFloorDigest.", digest, pinnedCursorRealityFloorDigest, dir)
	}
}

// pinnedCursorRealityFloorDigest is cursorRealityFloorDigest at the pinned
// beads (v1.3.0, internal/storage/schema/schema.go).
const pinnedCursorRealityFloorDigest = "65993bb205e8911af8e696360952bd16c8d8388a5db7c23c427e63e97dc14533"

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
		// Council pr2 E-S5 / E-I3: the shapes the D-F4 pin still skipped.
		{
			name: "a new sentinel KIND on ignoredSource",
			sources: map[string]string{"schema.go": header + strings.Replace(pinned,
				`cursorTable:     "ignored_schema_migrations",`,
				`cursorTable:     "ignored_schema_migrations",
		sentinelIndexes: []string{"idx_wisps_lease"},`, 1)},
			want: "unknown sentinel field sentinelIndexes",
		},
		{
			name: "a new field on migrationSource itself",
			sources: map[string]string{"schema.go": strings.Replace(header,
				`sentinelColumns []schemaSentinelColumn }`, `sentinelColumns []schemaSentinelColumn; realityIndexes []string }`, 1) + pinned},
			want: "migrationSource gained field realityIndexes",
		},
		{
			name: "a pointer literal third source",
			sources: map[string]string{
				"schema.go":  header + pinned,
				"sibling.go": "package schema\n\nvar thirdSource = &migrationSource{sentinelTables: []string{\"x\"}}\n",
			},
			want: "thirdSource",
		},
		{
			name: "a slice of sources",
			sources: map[string]string{
				"schema.go":  header + pinned,
				"sibling.go": "package schema\n\nvar extra = []migrationSource{{sentinelTables: []string{\"x\"}}}\n",
			},
			want: "extra[0]",
		},
		{
			name: "an init-time append in a sibling file",
			sources: map[string]string{
				"schema.go":  header + pinned,
				"sibling.go": "package schema\n\nfunc init() {\n\tignoredSource.sentinelTables = append(ignoredSource.sentinelTables, \"wisp_events\")\n}\n",
			},
			want: "sibling.go:4 references .sentinelTables outside cursorRealityFloor",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sources := map[string][]byte{}
			for name, body := range tc.sources {
				sources[name] = []byte(body)
			}
			_, found := readSentinelPin(sources)
			problems := strings.Join(found, "\n")
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

// TestCursorRealityFloorDigestSeesAClampArm proves the digest half of the pin
// (council pr2 E-S5): a new clamp arm changes it, and a comment or a reflow does
// not, so it fails on the beads bumps that matter and not on the ones that do
// not.
func TestCursorRealityFloorDigestSeesAClampArm(t *testing.T) {
	const base = `package schema

func (m migrationSource) cursorRealityFloor(ctx context.Context, db DBConn) (int, bool, error) {
	for _, table := range m.sentinelTables {
		present, err := sentinelTableExists(ctx, db, table)
		if err != nil {
			return 0, false, err
		}
		if !present {
			return 0, true, nil
		}
	}
	return 0, false, nil
}
`
	digestOf := func(t *testing.T, body string) string {
		t.Helper()
		parsed, err := parseSources(map[string][]byte{"schema.go": []byte(body)})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		digest, err := cursorRealityFloorDigest(parsed)
		if err != nil {
			t.Fatalf("digest: %v", err)
		}
		return digest
	}
	pinned := digestOf(t, base)

	reflowed := strings.Replace(base, "\t\tif !present {\n\t\t\treturn 0, true, nil\n\t\t}",
		"\t\t// A comment the digest must not see.\n\t\tif !present { return 0, true, nil }", 1)
	if reflowed == base {
		t.Fatal("the reflow row edited nothing")
	}
	if got := digestOf(t, reflowed); got != pinned {
		t.Errorf("a comment and a reflow changed the digest (%s -> %s); it would fail on edits that change nothing", pinned, got)
	}

	clamped := strings.Replace(base, "\treturn 0, false, nil\n}",
		"\tif m.cursorTable == \"ignored_schema_migrations\" {\n\t\treturn 20, true, nil\n\t}\n\treturn 0, false, nil\n}", 1)
	if clamped == base {
		t.Fatal("the clamp row edited nothing")
	}
	if got := digestOf(t, clamped); got == pinned {
		t.Error("a new clamp arm left the digest unchanged; the pin cannot see the library clamping a database gc admits")
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

// knownMigrationSourceFields is migrationSource's field set at the pinned
// beads. A field outside it is how a new sentinel KIND would arrive (council
// pr2 E-S5 / E-I3), so the pin refuses any addition until someone has re-read
// what cursorRealityFloor does with it.
var knownMigrationSourceFields = map[string]bool{
	"files": true, "dir": true, "cursorTable": true, "sentinelTables": true, "sentinelColumns": true,
}

// knownSentinelFields are the two sentinel kinds gc's probe reads.
var knownSentinelFields = map[string]bool{"sentinelTables": true, "sentinelColumns": true}

// parsedSources is the pinned package, parsed once, in a stable file order.
type parsedSources struct {
	fileSet *token.FileSet
	names   []string
	files   map[string]*ast.File
	bodies  map[string][]byte
}

func parseSources(sources map[string][]byte) (parsedSources, error) {
	parsed := parsedSources{fileSet: token.NewFileSet(), files: map[string]*ast.File{}, bodies: sources}
	for name := range sources {
		parsed.names = append(parsed.names, name)
	}
	sort.Strings(parsed.names)
	for _, name := range parsed.names {
		file, err := parser.ParseFile(parsed.fileSet, name, sources[name], 0)
		if err != nil {
			return parsed, err
		}
		parsed.files[name] = file
	}
	return parsed, nil
}

// readSentinelPin runs every structural check of the pin over sources and
// returns what the library declares plus one problem per disagreement or
// shape the pin cannot read. A shape it cannot read is a problem, never a skip:
// a pin that skipped what it could not parse is the one-directional pin again.
func readSentinelPin(sources map[string][]byte) (map[string]sentinelDecl, []string) {
	parsed, err := parseSources(sources)
	if err != nil {
		return nil, []string{"parse: " + err.Error()}
	}
	declared, problems := declaredSentinels(parsed)
	problems = append(problems, migrationSourceFieldProblems(parsed)...)
	problems = append(problems, sentinelMutationProblems(parsed)...)
	problems = append(problems, sentinelDrift(declared)...)
	return declared, problems
}

// declaredSentinels returns the sentinels of EVERY migrationSource literal in
// the package, wherever it sits: a package-level var (by value or by pointer),
// an element of a []migrationSource / [N]migrationSource / map[K]migrationSource
// literal with the type elided, or an assignment in a function body. The first
// version read only `var x = migrationSource{…}`, so a pointer literal or a slice
// of sources declared sentinels the pin never saw (council pr2 E-I3).
func declaredSentinels(parsed parsedSources) (map[string]sentinelDecl, []string) {
	declared := map[string]sentinelDecl{}
	var problems []string
	record := func(literal *ast.CompositeLit, name string) {
		d, err := sentinelsOf(literal)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			return
		}
		declared[name] = d
	}
	for _, fileName := range parsed.names {
		file := parsed.files[fileName]
		bound := map[*ast.CompositeLit]string{}
		for _, decl := range file.Decls {
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
					if i < len(value.Values) {
						if literal := unwrapLiteral(value.Values[i]); literal != nil {
							bound[literal] = ident.Name
						}
					}
				}
			}
		}
		nameOf := func(literal *ast.CompositeLit) string {
			if name, ok := bound[literal]; ok {
				return name
			}
			position := parsed.fileSet.Position(literal.Pos())
			return fmt.Sprintf("a migrationSource literal at %s:%d", filepath.Base(position.Filename), position.Line)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			switch {
			case isIdent(literal.Type, "migrationSource"):
				record(literal, nameOf(literal))
			case isSourceCollection(literal.Type):
				for i, element := range literal.Elts {
					value := element
					if kv, ok := element.(*ast.KeyValueExpr); ok {
						value = kv.Value
					}
					if inner := unwrapLiteral(value); inner != nil && inner.Type == nil {
						record(inner, fmt.Sprintf("%s[%d]", nameOf(literal), i))
					}
				}
			}
			return true
		})
	}
	return declared, problems
}

// unwrapLiteral returns the composite literal expr is, or points to.
func unwrapLiteral(expr ast.Expr) *ast.CompositeLit {
	switch value := expr.(type) {
	case *ast.CompositeLit:
		return value
	case *ast.UnaryExpr:
		if value.Op == token.AND {
			if literal, ok := value.X.(*ast.CompositeLit); ok {
				return literal
			}
		}
	}
	return nil
}

// isSourceCollection reports a slice, array or map type whose elements are
// migrationSource values or pointers.
func isSourceCollection(expr ast.Expr) bool {
	isSource := func(elem ast.Expr) bool {
		if star, ok := elem.(*ast.StarExpr); ok {
			elem = star.X
		}
		return isIdent(elem, "migrationSource")
	}
	switch collection := expr.(type) {
	case *ast.ArrayType:
		return isSource(collection.Elt)
	case *ast.MapType:
		return isSource(collection.Value)
	}
	return false
}

// migrationSourceFieldProblems pins migrationSource's field set.
func migrationSourceFieldProblems(parsed parsedSources) []string {
	var problems []string
	found := false
	for _, fileName := range parsed.names {
		ast.Inspect(parsed.files[fileName], func(node ast.Node) bool {
			spec, ok := node.(*ast.TypeSpec)
			if !ok || spec.Name.Name != "migrationSource" {
				return true
			}
			structType, ok := spec.Type.(*ast.StructType)
			if !ok {
				problems = append(problems, "migrationSource is no longer a struct")
				return false
			}
			found = true
			for _, field := range structType.Fields.List {
				for _, name := range field.Names {
					if !knownMigrationSourceFields[name.Name] {
						problems = append(problems, fmt.Sprintf("migrationSource gained field %s: a new field is how a "+
							"new sentinel kind arrives, and gc's probe reads only sentinelTables and sentinelColumns; "+
							"re-read cursorRealityFloor", name.Name))
					}
				}
				if len(field.Names) == 0 {
					problems = append(problems, "migrationSource gained an embedded field; re-read cursorRealityFloor")
				}
			}
			return false
		})
	}
	if !found {
		problems = append(problems, "the library declares no migrationSource struct")
	}
	return problems
}

// sentinelMutationProblems finds every reference to a sentinel field outside
// cursorRealityFloor, which is the only place the pinned library reads them.
// Anywhere else it is a write the literal pin cannot see — an init() that
// appends a table, a helper that reassigns the list — so it is a problem to
// re-read, not a detail (council pr2 E-S5).
func sentinelMutationProblems(parsed parsedSources) []string {
	var problems []string
	for _, fileName := range parsed.names {
		file := parsed.files[fileName]
		var allowed []*ast.BlockStmt
		for _, decl := range file.Decls {
			if function, ok := decl.(*ast.FuncDecl); ok && function.Name.Name == "cursorRealityFloor" && function.Body != nil {
				allowed = append(allowed, function.Body)
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok || !knownSentinelFields[selector.Sel.Name] {
				return true
			}
			for _, body := range allowed {
				if selector.Pos() >= body.Pos() && selector.End() <= body.End() {
					return true
				}
			}
			position := parsed.fileSet.Position(selector.Pos())
			problems = append(problems, fmt.Sprintf("%s:%d references .%s outside cursorRealityFloor; a write there "+
				"changes the sentinel set without changing the literal this pin reads",
				filepath.Base(position.Filename), position.Line, selector.Sel.Name))
			return true
		})
	}
	return problems
}

// cursorRealityFloorDigest is a digest of cursorRealityFloor's TOKENS, comments
// and layout excluded. The literal pin cannot see a new clamp arm
// (`return 20, true, nil` on some new condition) or a read of a field it does
// not know, and both change which cursor the library acts on; the digest
// changes with any of them, and with nothing a gofmt or a comment edit does.
func cursorRealityFloorDigest(parsed parsedSources) (string, error) {
	for _, fileName := range parsed.names {
		for _, decl := range parsed.files[fileName].Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Name.Name != "cursorRealityFloor" || function.Body == nil {
				continue
			}
			start := parsed.fileSet.Position(function.Pos()).Offset
			end := parsed.fileSet.Position(function.End()).Offset
			source := parsed.bodies[fileName][start:end]
			var tokens strings.Builder
			var scan scanner.Scanner
			scanFiles := token.NewFileSet()
			scan.Init(scanFiles.AddFile(fileName, -1, len(source)), source, nil, 0)
			for {
				_, tok, literal := scan.Scan()
				if tok == token.EOF {
					break
				}
				if tok == token.SEMICOLON {
					// Automatic semicolons follow line breaks, so they are
					// layout, not logic: `if c { return x }` and its three-line
					// form must digest alike.
					continue
				}
				tokens.WriteString(tok.String())
				tokens.WriteByte(' ')
				tokens.WriteString(literal)
				tokens.WriteByte('\n')
			}
			sum := sha256.Sum256([]byte(tokens.String()))
			return hex.EncodeToString(sum[:]), nil
		}
	}
	return "", errors.New("no cursorRealityFloor function")
}

// sentinelsOf reads the sentinel fields of one migrationSource literal. A
// field whose name says "sentinel" and is neither of the two gc reads is an
// error: the old switch had no default arm, so a new sentinel kind was skipped
// in silence (council pr2 E-S5 / E-I3).
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
		default:
			if name := keyName(field.Key); strings.Contains(strings.ToLower(name), "sentinel") {
				return d, fmt.Errorf("unknown sentinel field %s: gc's probe reads only sentinelTables and sentinelColumns", name)
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

// TestForwardDriftSeesOnlyTheMainLane pins the library facts the corrected A-F3
// residual on proxiedPinMemo rests on (council pr2 D-F7).
//
// The old residual said the library's CheckForwardDrift, run at every open,
// covered a database that moved inside the memo window. It covers one lane in
// one direction: CurrentVersion is the MAIN source's cursor, and
// checkSchemaSkew returns nil unless that cursor is AHEAD. So the ignored lane
// — the one A-F2 exists for — gets no library check before the open migrates it,
// and gc's own gate is the only one. A beads release that teaches the drift
// check the ignored lane breaks this test, which is the prompt to re-read the
// residual (and perhaps to shorten it).
func TestForwardDriftSeesOnlyTheMainLane(t *testing.T) {
	path := filepath.Join(beadstest.PinnedBeadsModuleDir(t), filepath.FromSlash("internal/storage/schema/schema.go"))
	body, err := os.ReadFile(path) //nolint:gosec // a path derived from the module cache
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, path, body, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	bodyOf := func(name string) string {
		t.Helper()
		for _, decl := range parsed.Decls {
			if function, ok := decl.(*ast.FuncDecl); ok && function.Recv == nil && function.Name.Name == name && function.Body != nil {
				return string(body[fileSet.Position(function.Body.Pos()).Offset:fileSet.Position(function.Body.End()).Offset])
			}
		}
		t.Fatalf("the pinned library no longer declares %s; re-read the A-F3 residual on proxiedPinMemo", name)
		return ""
	}

	if got := bodyOf("CheckForwardDrift"); !strings.Contains(got, "return checkSchemaSkew(ctx, db)") {
		t.Errorf("CheckForwardDrift is no longer checkSchemaSkew:\n%s", got)
	}
	skew := bodyOf("checkSchemaSkew")
	if !strings.Contains(skew, "CurrentVersion(ctx, db)") || strings.Contains(skew, "Ignored") {
		t.Errorf("checkSchemaSkew now reads something other than the main cursor; the residual says it does not:\n%s", skew)
	}
	if !strings.Contains(skew, "currentVersion <= LatestVersion()") {
		t.Errorf("checkSchemaSkew no longer passes a database at or behind the binary; the residual says it refuses AHEAD only:\n%s", skew)
	}
	if got := bodyOf("CurrentVersion"); !strings.Contains(got, "return mainSource.currentVersion(ctx, db)") {
		t.Errorf("CurrentVersion is no longer the MAIN source's cursor:\n%s", got)
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
