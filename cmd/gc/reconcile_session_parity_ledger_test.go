package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// The parity ledger for the legacy session pass (ga-zwddym6, from the audit
// ga-bw7yr7a): every step the legacy pass performs and v2 does not, and the
// tests that fail the build when a new one appears with no v2 counterpart.
//
// v2 either performs a step (an arm in rowArms), refuses it (a
// v2LatchRefusals entry in reconcile_latch.go) or records the difference: a
// parityAccepted, parityFindings or parityUnported entry in
// reconcile_session_differential_test.go, each of which demands a twin-world
// fixture, or a parityLegacyOnly row here, which has none. Three inventories
// keep that true:
//
//   - the parityLegacyOnly rows stay current (shape, legacy symbol still
//     declared, no v2 witness yet);
//   - every session field the legacy pass writes and no v2 site builds carries
//     a marker in the registry (internal/session/fields_legacy_only.go), whose
//     ledger and latch references this file checks;
//   - every event type a legacy-pass file emits has a v2 emitter or a cover.

// parityLegacyRow is one legacy-only step the differential fixtures cannot hold.
type parityLegacyRow struct {
	ID string // PAR-...
	// Legacy are the legacy symbols in cmd/gc, matched by declaration name
	// whatever the receiver. A symbol that is no longer declared means the
	// legacy step is gone and the row must go.
	Legacy []string
	// Witness are symbols whose appearance in a non-test v2 file (reconcile_*.go
	// or allocator*.go) proves a v2 counterpart exists. They are chosen so the
	// obvious fix trips them, so a fix PR deletes its row; that is the only way
	// its tests pass.
	Witness     []string
	V2Status    string // one line: what v2 does today
	Disposition string // "open" or "accepted"
	Owner       string // the bead that closes an open row
	Reason      string // an accepted row's reason, with its residual
}

const (
	legacyOnlyOpen     = "open"
	legacyOnlyAccepted = "accepted"
)

var parityLegacyOnly = []parityLegacyRow{}

var (
	parIDShape   = regexp.MustCompile(`^PAR-[A-Z0-9]+$`)
	parOwnerBead = regexp.MustCompile(`^(mc|ga)-[a-z0-9]+(\.[0-9]+)*$`)
)

// legacyOnlyShapeFindings is the table's shape rules: a PAR id, unique; legacy
// symbols and witnesses named; a status; an open row owned by a bead and an
// accepted one carrying a reason.
func legacyOnlyShapeFindings(rows []parityLegacyRow) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range rows {
		if !parIDShape.MatchString(r.ID) {
			out = append(out, fmt.Sprintf("%q: id must look like PAR-NAME", r.ID))
		}
		if seen[r.ID] {
			out = append(out, fmt.Sprintf("%s: duplicate id", r.ID))
		}
		seen[r.ID] = true
		if len(r.Legacy) == 0 || len(r.Witness) == 0 {
			out = append(out, fmt.Sprintf("%s: needs legacy symbols and v2 witness symbols", r.ID))
		}
		if strings.TrimSpace(r.V2Status) == "" {
			out = append(out, fmt.Sprintf("%s: no v2 status", r.ID))
		}
		switch r.Disposition {
		case legacyOnlyOpen:
			if !parOwnerBead.MatchString(r.Owner) {
				out = append(out, fmt.Sprintf("%s: open row's owner %q is not a bead id", r.ID, r.Owner))
			}
		case legacyOnlyAccepted:
			if strings.TrimSpace(r.Reason) == "" {
				out = append(out, fmt.Sprintf("%s: accepted row has no reason", r.ID))
			}
		default:
			out = append(out, fmt.Sprintf("%s: disposition %q is neither open nor accepted", r.ID, r.Disposition))
		}
	}
	return out
}

// legacyOnlyStaleFindings checks the two sides of every row against the tree:
// each legacy symbol is still declared in a non-test file, and no witness
// appears in a v2 file.
func legacyOnlyStaleFindings(rows []parityLegacyRow, files map[string]*ast.File) []string {
	declared := map[string]bool{}
	v2Idents := map[string]string{}
	for name, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				declared[fd.Name.Name] = true
			}
		}
		if !isV2SourceFile(name) {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				v2Idents[id.Name] = name
			}
			return true
		})
	}
	var out []string
	for _, r := range rows {
		for _, s := range r.Legacy {
			if !declared[s] {
				out = append(out, fmt.Sprintf("%s: legacy symbol %s is no longer declared in a non-test cmd/gc file; the legacy step is gone, delete the row", r.ID, s))
			}
		}
		for _, w := range r.Witness {
			if file, ok := v2Idents[w]; ok {
				out = append(out, fmt.Sprintf("%s: witness %s appears in %s; v2 has a counterpart, delete the row", r.ID, w, file))
			}
		}
	}
	return out
}

// isV2SourceFile is whether a cmd/gc file belongs to the v2 reconciler:
// reconcile_*.go and allocator*.go.
func isV2SourceFile(name string) bool {
	return strings.HasPrefix(name, "reconcile_") || strings.HasPrefix(name, "allocator")
}

var (
	gcSourceOnce  sync.Once
	gcSourceFiles map[string]*ast.File
	gcSourceErr   error
)

// nonTestGCFiles parses every non-test Go file of cmd/gc, keyed by base name.
func nonTestGCFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	gcSourceOnce.Do(func() {
		_, currentFile, _, ok := runtime.Caller(0)
		if !ok {
			gcSourceErr = fmt.Errorf("runtime.Caller failed")
			return
		}
		dir := gcCallerDir(currentFile)
		entries, err := os.ReadDir(dir)
		if err != nil {
			gcSourceErr = err
			return
		}
		gcSourceFiles = map[string]*ast.File{}
		fset := token.NewFileSet()
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
			if err != nil {
				gcSourceErr = err
				return
			}
			gcSourceFiles[name] = f
		}
	})
	if gcSourceErr != nil {
		t.Fatalf("reading cmd/gc sources: %v", gcSourceErr)
	}
	return gcSourceFiles
}

func parseSource(t *testing.T, name, src string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	return f
}

// TestParityLegacyOnlyTable checks the ledger rows' shape and that neither
// side of any row is stale.
func TestParityLegacyOnlyTable(t *testing.T) {
	for _, f := range append(legacyOnlyShapeFindings(parityLegacyOnly), legacyOnlyStaleFindings(parityLegacyOnly, nonTestGCFiles(t))...) {
		t.Error(f)
	}
}

// TestParityLegacyOnlyTableCatches mutates the table the ways its rules exist
// to catch and checks each mutant fails with its reason.
func TestParityLegacyOnlyTableCatches(t *testing.T) {
	files := map[string]*ast.File{
		"legacy_pass.go":       parseSource(t, "legacy_pass.go", "package main\ntype d struct{}\nfunc (*d) legacyStep() {}\nfunc plainStep() {}\n"),
		"reconcile_v2_step.go": parseSource(t, "reconcile_v2_step.go", "package main\nfunc armV2() { witnessed() }\n"),
		"not_v2_witness.go":    parseSource(t, "not_v2_witness.go", "package main\nfunc other() { elsewhere() }\n"),
	}
	good := parityLegacyRow{ID: "PAR-X", Legacy: []string{"legacyStep"}, Witness: []string{"neverCalled"}, V2Status: "none", Disposition: legacyOnlyOpen, Owner: "ga-abc123"}
	mut := func(fn func(*parityLegacyRow)) []parityLegacyRow {
		r := good
		fn(&r)
		return []parityLegacyRow{r}
	}
	if got := append(legacyOnlyShapeFindings([]parityLegacyRow{good}), legacyOnlyStaleFindings([]parityLegacyRow{good}, files)...); len(got) != 0 {
		t.Fatalf("the good row fails: %v", got)
	}
	for _, tc := range []struct {
		name string
		rows []parityLegacyRow
		want string
	}{
		{"no PAR prefix", mut(func(r *parityLegacyRow) { r.ID = "DRAINDL" }), "id must look like PAR-NAME"},
		{"duplicate id", append([]parityLegacyRow{good}, good), "duplicate id"},
		{"no witness", mut(func(r *parityLegacyRow) { r.Witness = nil }), "needs legacy symbols and v2 witness symbols"},
		{"no status", mut(func(r *parityLegacyRow) { r.V2Status = " " }), "no v2 status"},
		{"malformed owner", mut(func(r *parityLegacyRow) { r.Owner = "the C5b follow-up PR" }), "is not a bead id"},
		{"accepted without a reason", mut(func(r *parityLegacyRow) { r.Disposition, r.Owner = legacyOnlyAccepted, "" }), "accepted row has no reason"},
		{"unknown disposition", mut(func(r *parityLegacyRow) { r.Disposition = "later" }), "neither open nor accepted"},
		{"legacy symbol gone", mut(func(r *parityLegacyRow) { r.Legacy = []string{"nonexistentStep"} }), "legacy symbol nonexistentStep is no longer declared"},
		{"witness in a v2 file", mut(func(r *parityLegacyRow) { r.Witness = []string{"witnessed"} }), "witness witnessed appears in reconcile_v2_step.go"},
	} {
		got := strings.Join(append(legacyOnlyShapeFindings(tc.rows), legacyOnlyStaleFindings(tc.rows, files)...), "\n")
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: mutant passed or failed otherwise; want %q in:\n%s", tc.name, tc.want, got)
		}
	}
	// A witness that only a non-v2 file uses is no counterpart; a method is
	// matched by its declaration name.
	row := parityLegacyRow{ID: "PAR-Y", Legacy: []string{"legacyStep", "plainStep"}, Witness: []string{"elsewhere"}, V2Status: "none", Disposition: legacyOnlyOpen, Owner: "mc-1"}
	if got := legacyOnlyStaleFindings([]parityLegacyRow{row}, files); len(got) != 0 {
		t.Errorf("a method symbol or a non-v2 witness use is reported stale: %v", got)
	}
}

// latchLiteral is the shape of a PAR id.
var (
	latchLiteral = regexp.MustCompile(`^PAR-[A-Z0-9]+(-[A-Z0-9]+)*$`)
)

// latchPARIDs is every PAR id a v2LatchRefusals entry names: a string literal
// of that shape in reconcile_latch.go.
func latchPARIDs(files map[string]*ast.File) map[string]bool {
	ids := map[string]bool{}
	f := files["reconcile_latch.go"]
	if f == nil {
		return ids
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
			if s := strings.Trim(bl.Value, "\"`"); latchLiteral.MatchString(s) {
				ids[s] = true
			}
		}
		return true
	})
	return ids
}

// ledgerIDs is every id the ledger answers to: a parityLegacyOnly row's PAR id
// or a parityFindings name.
func ledgerIDs() map[string]bool {
	ids := map[string]bool{}
	for _, r := range parityLegacyOnly {
		ids[r.ID] = true
	}
	for name := range parityFindings {
		ids[name] = true
	}
	return ids
}

// coverFindings checks one marker's reference where the registry package
// cannot: a ledger reference names a ledger row, a latch reference names a
// PAR id v2LatchRefusals carries. Other kinds are checked by shape alone.
func coverFindings(subject, marker string, ledger, latch map[string]bool) []string {
	kind, ref, _ := strings.Cut(marker, ":")
	switch kind {
	case "ledger":
		if !ledger[ref] {
			return []string{fmt.Sprintf("%s: marker %q names no ledger row (a parityLegacyOnly id or a parityFindings entry)", subject, marker)}
		}
	case "latch":
		if !latch[ref] {
			return []string{fmt.Sprintf("%s: marker %q names no v2LatchRefusals PAR id", subject, marker)}
		}
	}
	return nil
}

// TestLegacyOnlyRegistryMarkersResolve checks every registry marker that
// refers to the ledger or the latch refers to something that exists.
func TestLegacyOnlyRegistryMarkersResolve(t *testing.T) {
	ledger, latch := ledgerIDs(), latchPARIDs(nonTestGCFiles(t))
	for key, marker := range session.LegacyOnlyMarkers() {
		for _, f := range coverFindings(key, marker, ledger, latch) {
			t.Error(f)
		}
	}
}

// TestCoverFindingsCatches is the negative test for the reference check.
func TestCoverFindingsCatches(t *testing.T) {
	ledger, latch := map[string]bool{"PAR-OK": true}, map[string]bool{"PAR-AGE": true}
	for _, tc := range []struct{ marker, want string }{
		{"ledger:PAR-GONE", "names no ledger row"},
		{"latch:PAR-GONE", "names no v2LatchRefusals PAR id"},
	} {
		if got := strings.Join(coverFindings("k", tc.marker, ledger, latch), ""); !strings.Contains(got, tc.want) {
			t.Errorf("%s: want %q, got %q", tc.marker, tc.want, got)
		}
	}
	for _, ok := range []string{"ledger:PAR-OK", "latch:PAR-AGE", "accepted:a reason"} {
		if got := coverFindings("k", ok, ledger, latch); len(got) != 0 {
			t.Errorf("%s reported: %v", ok, got)
		}
	}
}

// Event inventory. A legacy-pass file's event emission is an
// events.Event literal whose Type is an events.* constant; a constant only
// read in a filter is not an emission. The scan sees literals only: an
// emission that passes the type as a function argument or through a variable
// is not seen, which is why a legacy-pass file whose literal Type is not an
// events.* selector fails instead of passing silently.

const (
	fileLegacy  = "legacy-pass" // emitted only by the legacy session pass
	fileOutside = "outside"     // the CLI, API, orders, mail, storage and the other lanes; or shared with v2
)

// eventFileClass classifies every cmd/gc file that constructs an events.Event
// with a Type, other than the v2 files (reconcile_*.go, allocator*.go), which
// are v2 by name. A new emitting file nobody classified fails
// TestLegacyPassEventsHaveACounterpart.
var eventFileClass = map[string]string{}

// legacyEventCover maps an events.* constant a legacy-pass file emits and no
// v2 file does to what covers it: "ledger:<id>", "latch:<PAR id>",
// "accepted:<reason>" or "ruling-pending:<bead>: <note>", as the registry's
// markers read.
var legacyEventCover = map[string]string{}

// eventEmission is what one file's literals say.
type eventEmission struct {
	types      map[string]bool // events.* constants
	unresolved int             // literals whose Type is not an events.* selector
}

// scanEventEmissions finds, per file, the Type of every events.Event literal.
func scanEventEmissions(files map[string]*ast.File) map[string]*eventEmission {
	out := map[string]*eventEmission{}
	for name, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := cl.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Event" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "events" {
				return true
			}
			for _, e := range cl.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "Type" {
					continue
				}
				em := out[name]
				if em == nil {
					em = &eventEmission{types: map[string]bool{}}
					out[name] = em
				}
				if v, ok := kv.Value.(*ast.SelectorExpr); ok {
					if pkg, ok := v.X.(*ast.Ident); ok && pkg.Name == "events" {
						em.types[v.Sel.Name] = true
						continue
					}
				}
				em.unresolved++
			}
			return true
		})
	}
	return out
}

// eventInventoryFindings is the event rules: every emitting file classified
// once (v2 files by name); a legacy-pass file's literals all resolve; every
// type a legacy-pass file emits has a v2 emitter or a cover, and a cover goes
// when the type gains a v2 emitter or loses its legacy one.
func eventInventoryFindings(emissions map[string]*eventEmission, class, cover map[string]string, ledger, latch map[string]bool) []string {
	var out []string
	names := make([]string, 0, len(emissions))
	for name := range emissions {
		names = append(names, name)
	}
	sort.Strings(names)
	v2Types, legacyTypes := map[string]bool{}, map[string]string{}
	for _, name := range names {
		em := emissions[name]
		c, classified := class[name]
		switch {
		case isV2SourceFile(name):
			if classified {
				out = append(out, fmt.Sprintf("%s: a v2 file is classified by its name, not in eventFileClass", name))
			}
			for t := range em.types {
				v2Types[t] = true
			}
			continue
		case !classified:
			out = append(out, fmt.Sprintf("%s: emits events but is in no class; add it to eventFileClass as %s or %s", name, fileLegacy, fileOutside))
			continue
		case c != fileLegacy && c != fileOutside:
			out = append(out, fmt.Sprintf("%s: class %q is neither %s nor %s", name, c, fileLegacy, fileOutside))
			continue
		}
		if c != fileLegacy {
			continue
		}
		if em.unresolved > 0 {
			out = append(out, fmt.Sprintf("%s: %d events.Event literal(s) whose Type is not an events.* constant; the scan cannot name what this legacy-pass file emits", name, em.unresolved))
		}
		for t := range em.types {
			legacyTypes[t] = name
		}
	}
	for name := range class {
		if emissions[name] == nil {
			out = append(out, fmt.Sprintf("%s: classified in eventFileClass but constructs no events.Event with a Type; delete the entry", name))
		}
	}
	covered := make([]string, 0, len(cover))
	for t := range cover {
		covered = append(covered, t)
	}
	sort.Strings(covered)
	for _, t := range covered {
		switch file, legacyEmits := legacyTypes[t]; {
		case !legacyEmits:
			out = append(out, fmt.Sprintf("events.%s: has a cover but no legacy-pass file emits it; delete the cover", t))
		case v2Types[t]:
			out = append(out, fmt.Sprintf("events.%s: covered, but a v2 file now emits it too (legacy: %s); delete the cover", t, file))
		default:
			out = append(out, coverFindings("events."+t, cover[t], ledger, latch)...)
			if err := validMarker(cover[t]); err != "" {
				out = append(out, fmt.Sprintf("events.%s: %s", t, err))
			}
		}
	}
	legacyList := make([]string, 0, len(legacyTypes))
	for t := range legacyTypes {
		legacyList = append(legacyList, t)
	}
	sort.Strings(legacyList)
	for _, t := range legacyList {
		if !v2Types[t] && cover[t] == "" {
			out = append(out, fmt.Sprintf("events.%s: emitted by legacy-pass file %s, no v2 file emits it and no ledger row covers it; give it a v2 emitter or a legacyEventCover entry", t, legacyTypes[t]))
		}
	}
	return out
}

// validMarker is the shape rule for the marker kinds the registry shares.
func validMarker(m string) string {
	kind, rest, _ := strings.Cut(m, ":")
	switch kind {
	case "ledger", "latch":
		if strings.TrimSpace(rest) == "" {
			return fmt.Sprintf("marker %q names nothing", m)
		}
	case "accepted":
		if strings.TrimSpace(rest) == "" {
			return fmt.Sprintf("marker %q has no reason", m)
		}
	case "ruling-pending":
		bead, note, _ := strings.Cut(rest, ": ")
		if !parOwnerBead.MatchString(strings.TrimSpace(bead)) || strings.TrimSpace(note) == "" {
			return fmt.Sprintf("marker %q needs a bead id and a note", m)
		}
	default:
		return fmt.Sprintf("marker %q has no known kind", m)
	}
	return ""
}

// TestLegacyPassEventsHaveACounterpart is inventory test (b): every event type
// the legacy session pass emits is emitted by v2 or covered by the ledger.
func TestLegacyPassEventsHaveACounterpart(t *testing.T) {
	for _, f := range eventInventoryFindings(scanEventEmissions(nonTestGCFiles(t)), eventFileClass, legacyEventCover, ledgerIDs(), latchPARIDs(nonTestGCFiles(t))) {
		t.Error(f)
	}
}

// TestEventInventoryCatches is the negative test: each mutant of a synthetic
// tree fails with its reason.
func TestEventInventoryCatches(t *testing.T) {
	src := func(typ string) string {
		return "package main\nimport \"x/events\"\nfunc f() { _ = events.Event{Type: " + typ + "} }\n"
	}
	tree := func(extra map[string]string) map[string]*eventEmission {
		files := map[string]*ast.File{
			"legacy_pass.go":   parseSource(t, "legacy_pass.go", src("events.OnlyLegacy")),
			"shared.go":        parseSource(t, "shared.go", src("events.Shared")),
			"reconcile_arm.go": parseSource(t, "reconcile_arm.go", src("events.Shared")),
		}
		for name, s := range extra {
			files[name] = parseSource(t, name, s)
		}
		return scanEventEmissions(files)
	}
	class := func(extra map[string]string) map[string]string {
		c := map[string]string{"legacy_pass.go": fileLegacy, "shared.go": fileOutside}
		for k, v := range extra {
			c[k] = v
		}
		return c
	}
	ledger, latch := map[string]bool{"PAR-OK": true}, map[string]bool{"PAR-AGE": true}
	cover := func(m string) map[string]string { return map[string]string{"OnlyLegacy": m} }
	find := func(em map[string]*eventEmission, c, cv map[string]string) string {
		return strings.Join(eventInventoryFindings(em, c, cv, ledger, latch), "\n")
	}
	if got := find(tree(nil), class(nil), cover("ledger:PAR-OK")); got != "" {
		t.Fatalf("the good tree fails:\n%s", got)
	}
	for _, tc := range []struct {
		name, got, want string
	}{
		{"unclassified emitting file", find(tree(map[string]string{"new_emitter.go": src("events.Brand")}), class(nil), cover("ledger:PAR-OK")), "new_emitter.go: emits events but is in no class"},
		{"legacy type with no v2 emitter and no cover", find(tree(nil), class(nil), nil), "events.OnlyLegacy: emitted by legacy-pass file legacy_pass.go"},
		{"cover names no ledger row", find(tree(nil), class(nil), cover("ledger:PAR-GONE")), "names no ledger row"},
		{"cover of malformed shape", find(tree(nil), class(nil), cover("ruling-pending:later")), "needs a bead id and a note"},
		{"stale cover: v2 now emits it", find(tree(map[string]string{"reconcile_new.go": src("events.OnlyLegacy")}), class(nil), cover("ledger:PAR-OK")), "a v2 file now emits it too"},
		{"stale cover: legacy no longer emits it", find(tree(nil), class(nil), map[string]string{"OnlyLegacy": "ledger:PAR-OK", "Gone": "ledger:PAR-OK"}), "events.Gone: has a cover but no legacy-pass file emits it"},
		{"unresolvable legacy literal", find(tree(map[string]string{"legacy_var.go": src("kind")}), class(map[string]string{"legacy_var.go": fileLegacy}), cover("ledger:PAR-OK")), "legacy_var.go: 1 events.Event literal(s) whose Type is not an events.* constant"},
		{"classified file that emits nothing", find(tree(nil), class(map[string]string{"quiet.go": fileOutside}), cover("ledger:PAR-OK")), "quiet.go: classified in eventFileClass but constructs no events.Event"},
		{"v2 file in the table", find(tree(nil), class(map[string]string{"reconcile_arm.go": fileOutside}), cover("ledger:PAR-OK")), "a v2 file is classified by its name"},
	} {
		if !strings.Contains(tc.got, tc.want) {
			t.Errorf("%s: mutant passed or failed otherwise; want %q in:\n%s", tc.name, tc.want, tc.got)
		}
	}
	// A constant only read in a filter is not an emission.
	read := scanEventEmissions(map[string]*ast.File{"filter.go": parseSource(t, "filter.go", "package main\nimport \"x/events\"\nfunc f() { _ = events.Filter{Type: events.OnlyLegacy} }\n")})
	if len(read) != 0 {
		t.Errorf("a filter's Type is scanned as an emission: %v", read)
	}
}
