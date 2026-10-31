package storeclass_test

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/internal/analyzertest"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/storeclass"
)

const module = "example.com/m/"

const beadsSrc = `package beads

type Bead struct{ ID string }

type Store interface{ Get(id string) (Bead, error) }

type Counter interface{ Count() int }

type CachingStore struct{ Store }

type WorkStore struct{ Store }

type GraphStore struct{ Store }

func (s WorkStore) Raw() Store  { return s.Store }
func (s GraphStore) Raw() Store { return s.Store }

func (s GraphStore) ConditionalWritesResolveTarget() Store { return s.Store }

func MintClassStores(s Store) (WorkStore, GraphStore) { return WorkStore{s}, GraphStore{s} }

func FederatedRead(s Store) Store { return s }

func As[T any](s Store) (T, bool) { v, ok := s.(T); return v, ok }
`

const beadstestSrc = `package beadstest

import "example.com/m/internal/beads"

func Work(s beads.Store) beads.WorkStore { return beads.WorkStore{Store: s} }
`

// storerefSrc is allowlisted for every rule but cap-assert.
const storerefSrc = `package storeref

import "example.com/m/internal/beads"

type Leg struct{ Store beads.Store }

func Mint(s beads.Store) beads.GraphStore { _, g := beads.MintClassStores(s); return g }

func Probe(s beads.Store) beads.Store { return beads.FederatedRead(s) }

func peek(s beads.Store) bool { _, ok := s.(beads.Counter); return ok } // cap-assert
`

// callers' trailing comments name the rules each line must report, once per
// occurrence.
const callers = `package gc

import (
	"example.com/m/internal/beads"
	"example.com/m/internal/beads/beadstest" // test-mint-import
)

type holder struct {
	store  beads.Store             // raw-field
	stores []beads.Store           // raw-field
	open   func() beads.Store      // raw-field
	graph  beads.GraphStore
	inner  struct{ s beads.Store } // raw-field
	count  int
}

type opener func(string) beads.Store // raw-type

type rawIface interface { beads.Store; Extra() } // raw-type

type alias = beads.Store // raw-type

type typedMap map[string]beads.GraphStore

var cityStore beads.Store // raw-var

var cityStores, rigStores map[string]beads.Store // raw-var raw-var

func param(s beads.Store) {} // raw-param

func two(a, b beads.Store) {} // raw-param raw-param

func result() beads.Store { return nil } // raw-result

func viaMap(m map[string]beads.Store, p *beads.Store) {} // raw-param raw-param

func generic(p chan []beads.Store) {} // raw-param

func typed(g beads.GraphStore, w beads.WorkStore, m typedMap) {}

func lit() { f := func(s beads.Store) beads.Store { return s }; _ = f } // raw-param raw-result

type rigs struct{}

func (rigs) method(s beads.Store) {} // raw-param

type box[T any] struct{ v T }

func (b *box[T]) put(s beads.Store) {} // raw-param

func ifaceMethod() { var _ interface{ Open(beads.Store) } } // raw-param

func localType() { type local struct{ s beads.Store }; _ = local{} } // raw-field

func mint(g beads.GraphStore) beads.WorkStore { return beads.WorkStore{Store: g.Store} } // mint unwrap

func convert(w beads.WorkStore) beads.GraphStore { return beads.GraphStore(w) } // mint

func mintCall(g beads.GraphStore) { beads.MintClassStores(g) } // mint

func mintValue() { f := beads.MintClassStores; _ = f } // mint

func zero() beads.GraphStore { return beads.GraphStore{} }

func elided(g beads.GraphStore) []beads.WorkStore { return []beads.WorkStore{{g}} } // mint

func pointer(g beads.GraphStore) *beads.WorkStore { return &beads.WorkStore{g} } // mint

func unwrapped(g beads.GraphStore) { _ = g.Store } // unwrap

func raw(g beads.GraphStore) { _ = g.Raw() } // unwrap

func rawValue(g beads.GraphStore) { f := g.Raw; _ = f } // unwrap

type embeds struct{ beads.GraphStore }

func promoted(e embeds) { _ = e.Store } // unwrap

func methodOK(g beads.GraphStore) { _, _ = g.Get("x") }

func upcastOK(g beads.GraphStore) { var s interface{ Get(string) (beads.Bead, error) } = g; _ = s }

func assert(w beads.WorkStore) { _, _ = w.Store.(beads.Counter) } // unwrap cap-assert

func assertAnon(g beads.GraphStore) { s := g.Raw(); _, _ = s.(interface{ Count() int }) } // unwrap cap-assert

func assertCaching(w beads.WorkStore) { var s beads.Store = w; _, _ = s.(*beads.CachingStore) } // cap-assert

func assertSwitch(w beads.WorkStore) { var s beads.Store = w; switch s.(type) { case beads.Counter: } } // cap-assert

func assertBoxed(g beads.GraphStore) { _, _ = any(g).(beads.Counter) } // cap-assert

func assertBoxedConcrete(c *beads.CachingStore) { _, _ = any(c).(beads.Counter) } // raw-param cap-assert

func assertWide(s rawIface) { _, _ = s.(beads.Counter) } // raw-param cap-assert

func assertOther(x any) { _, _ = x.(beads.Counter) }

func asOK(g beads.GraphStore) { _, _ = beads.As[beads.Counter](g) }

func marker(g beads.GraphStore) { beads.FederatedRead(g) } // read-marker

func testMint(g beads.GraphStore) beads.WorkStore { return beadstest.Work(g) }

func constrained[S beads.Store](s S) {} // raw-type

type boxed[S beads.Store] struct{ s S } // raw-type

func unionC[S interface{ *beads.CachingStore | int }]() {} // raw-type

func unionBare[S *beads.CachingStore | int](s S) {} // raw-type

func tildeC[S ~*beads.CachingStore]() {} // raw-type

type unionBox[S *beads.CachingStore | int,] struct{ s S } // raw-type

type tildeBox[S ~*beads.CachingStore] struct{} // raw-type

func concrete(c *beads.CachingStore) []beads.CachingStore { return nil } // raw-param raw-result

func target(g beads.GraphStore) { _ = g.ConditionalWritesResolveTarget() } // unwrap

func asRaw(g beads.GraphStore) { _, _ = beads.As[*beads.CachingStore](g) } // unwrap

func asStore(g beads.GraphStore) { _, _ = beads.As[beads.Store](g) } // unwrap

func embedsOK(e embeds) {}
`

// bootSrc is an allowlisted file: nothing in it reports.
const bootSrc = `package gc

import "example.com/m/internal/beads"

func boot(s beads.Store) beads.GraphStore { return beads.GraphStore{Store: s} }
`

// migrateSrc is allowlisted for the raw-* rules only.
const migrateSrc = `package gc

import "example.com/m/internal/beads"

func migrate(s beads.Store) beads.GraphStore { return beads.GraphStore{Store: s} } // mint
`

// generatedSrc carries a generated-code header, and genNamedSrc is in a
// _gen.go file: both are linted like any other file.
const generatedSrc = `// Code generated by a tool; DO NOT EDIT.

package gc

import "example.com/m/internal/beads"

func generated(s beads.Store) {} // raw-param
`

const genNamedSrc = `package gc

import "example.com/m/internal/beads"

func genNamed(s beads.Store) {} // raw-param
`

const fixtureTest = `package gc

import "example.com/m/internal/beads"

func fixture(s beads.Store) beads.GraphStore { return beads.GraphStore{Store: s} }
`

func config() storeclass.Config {
	return storeclass.Config{
		Module:           module,
		Store:            module + "internal/beads.Store",
		Handles:          []string{module + "internal/beads.WorkStore", module + "internal/beads.GraphStore"},
		HandleField:      "Store",
		UnwrapMethods:    []string{"Raw", "ConditionalWritesResolveTarget"},
		MintFuncs:        []string{module + "internal/beads.MintClassStores"},
		TestMintPackages: []string{module + "internal/beads/beadstest"},
		Markers:          []string{module + "internal/beads.FederatedRead"},
		Allow: []storeclass.Allow{
			{Package: "internal/beads", Reason: "the engines and handles"},
			{Package: "internal/storeref", Rules: storeclass.AllRulesBut(storeclass.CapAssert), Reason: "the mint and executors"},
			{Package: "cmd/gc", File: "storage_boot.go", Reason: "boot routing"},
			{Package: "cmd/gc", File: "infra_class_migrate.go", Rules: []storeclass.Rule{storeclass.RawParam, storeclass.RawResult, storeclass.RawField, storeclass.RawVar, storeclass.RawType}, Reason: "the migration engine"},
		},
		AsFuncs: []string{module + "internal/beads.As"},
	}
}

func graph(callersSrc string) []analyzertest.Package {
	return []analyzertest.Package{
		{Path: module + "internal/beads", Files: map[string]string{"beads.go": beadsSrc}},
		{Path: module + "internal/beads/beadstest", Files: map[string]string{"beadstest.go": beadstestSrc}},
		{Path: module + "internal/storeref", Files: map[string]string{"storeref.go": storerefSrc}},
		{Path: module + "cmd/gc", Files: map[string]string{
			"callers.go":             callersSrc,
			"storage_boot.go":        bootSrc,
			"infra_class_migrate.go": migrateSrc,
			"zz_generated.go":        generatedSrc,
			"zz_census_gen.go":       genNamedSrc,
			"fixture_test.go":        fixtureTest,
		}},
	}
}

var ruleRE = regexp.MustCompile(`^storeclass\[([a-z-]+)\]`)

// findings is "file:line rule" for every diagnostic, sorted.
func findings(t *testing.T, cfg storeclass.Config) []string {
	t.Helper()
	var got []string
	for _, d := range analyzertest.RunGraph(t, storeclass.New(cfg), graph(callers)) {
		m := ruleRE.FindStringSubmatch(d.Message)
		if m == nil {
			got = append(got, fmt.Sprintf("%s:%d ? %s", d.File, d.Line, d.Message))
			continue
		}
		got = append(got, fmt.Sprintf("%s:%d %s", d.File, d.Line, m[1]))
	}
	sort.Strings(got)
	return got
}

// expected is "file:line rule" for every rule a trailing comment of src
// names, once per name.
func expected(file, src string) []string {
	var want []string
	for i, line := range strings.Split(src, "\n") {
		_, comment, ok := strings.Cut(line, "// ")
		if !ok {
			continue
		}
		for _, f := range strings.Fields(comment) {
			if slices.Contains(storeclass.Rules, storeclass.Rule(f)) {
				want = append(want, fmt.Sprintf("%s:%d %s", file, i+1, f))
			}
		}
	}
	return want
}

func TestStoreclassRules(t *testing.T) {
	got := findings(t, config())
	want := slices.Concat(expected("callers.go", callers), expected("infra_class_migrate.go", migrateSrc),
		expected("zz_generated.go", generatedSrc), expected("zz_census_gen.go", genNamedSrc))
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("findings differ\n got: %s\nwant: %s", strings.Join(got, "\n      "), strings.Join(want, "\n      "))
	}
}

// lineSrc names itself a test file with a //line directive, which renames it
// for every position after it.
const lineSrc = `//line zz_hidden_test.go:1
package gc

import "example.com/m/internal/beads"

func renamed(s beads.Store) {}
`

// TestStoreclassLintsEveryBuiltFile: no mark a built file can carry takes it
// out of the lint, nor out of the census the baseline generator collects: a
// generated-code header, a _gen.go name, or a //line directive that names it
// a test file. Each would let a baselined file's rows shrink, which needs no
// label, and then hide every later raw store in it.
func TestStoreclassLintsEveryBuiltFile(t *testing.T) {
	g := graph(callers)
	g[len(g)-1].Files["zz_line.go"] = lineSrc
	var msgs []string
	for _, d := range analyzertest.RunGraph(t, storeclass.New(config()), g) {
		msgs = append(msgs, d.Message)
	}
	counted := map[storeclass.Key]int{}
	analyzertest.RunGraph(t, storeclass.NewCollector(config(), func(f storeclass.Finding) { counted[f.Key]++ }), g)
	for _, k := range []storeclass.Key{
		{Path: "cmd/gc/zz_generated.go", Func: "generated", Rule: storeclass.RawParam},
		{Path: "cmd/gc/zz_census_gen.go", Func: "genNamed", Rule: storeclass.RawParam},
		{Path: "cmd/gc/zz_line.go", Func: "renamed", Rule: storeclass.RawParam},
	} {
		if !slices.ContainsFunc(msgs, func(m string) bool { return strings.Contains(m, "("+k.Path+" "+k.Func+": 1 here") }) {
			t.Errorf("%s: not reported", k)
		}
		if counted[k] != 1 {
			t.Errorf("the census counted %d of %s, want 1", counted[k], k)
		}
	}
}

// storerefGraph analyzes the allowlisted storeref package (analyzertest runs
// the analyzer on the graph's last package).
func storerefGraph() []analyzertest.Package {
	return graph(callers)[:3]
}

// TestStoreclassAllowlistByRule: storeref is allowlisted for every rule but
// cap-assert.
func TestStoreclassAllowlistByRule(t *testing.T) {
	var got []string
	for _, d := range analyzertest.RunGraph(t, storeclass.New(config()), storerefGraph()) {
		m := ruleRE.FindStringSubmatch(d.Message)
		if m == nil {
			t.Fatalf("unexpected finding %s:%d %s", d.File, d.Line, d.Message)
		}
		got = append(got, fmt.Sprintf("%s:%d %s", d.File, d.Line, m[1]))
	}
	want := expected("storeref.go", storerefSrc)
	if !slices.Equal(got, want) {
		t.Errorf("findings = %v, want %v", got, want)
	}
}

func TestStoreclassEveryRuleExercised(t *testing.T) {
	seen := map[storeclass.Rule]bool{}
	for _, w := range append(expected("callers.go", callers), expected("storeref.go", storerefSrc)...) {
		seen[storeclass.Rule(w[strings.LastIndex(w, " ")+1:])] = true
	}
	for _, r := range storeclass.Rules {
		if !seen[r] {
			t.Errorf("rule %s has no fixture line", r)
		}
	}
}

// TestStoreclassMessages pins the message shape a developer sees.
func TestStoreclassMessages(t *testing.T) {
	var msgs []string
	for _, d := range analyzertest.RunGraph(t, storeclass.New(config()), graph(callers)) {
		if d.File == "callers.go" && d.Line == lineOf(callers, "func param(") {
			msgs = append(msgs, d.Message)
		}
	}
	if len(msgs) != 1 {
		t.Fatalf("param: %d findings, want 1: %v", len(msgs), msgs)
	}
	for _, want := range []string{"storeclass[raw-param]", "parameter s", "raw beads.Store", "cmd/gc/callers.go", "param", "1 here, baseline 0"} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("message %q lacks %q", msgs[0], want)
		}
	}
}

func lineOf(src, prefix string) int {
	for i, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, prefix) {
			return i + 1
		}
	}
	return -1
}

func TestStoreclassBaseline(t *testing.T) {
	cfg := config()
	cfg.Baseline = map[storeclass.Key]int{
		// pinned at the tree: silent.
		{Path: "cmd/gc/callers.go", Func: "two", Rule: storeclass.RawParam}:            2,
		{Path: "cmd/gc/callers.go", Func: "rigs.method", Rule: storeclass.RawParam}:    1,
		{Path: "cmd/gc/callers.go", Func: "box.put", Rule: storeclass.RawParam}:        1,
		{Path: "cmd/gc/callers.go", Func: "cityStores", Rule: storeclass.RawVar}:       1,
		{Path: "cmd/gc/callers.go", Func: "rigStores", Rule: storeclass.RawVar}:        1,
		{Path: "cmd/gc/callers.go", Func: "lit", Rule: storeclass.RawResult}:           1,
		{Path: "cmd/gc/callers.go", Func: "(import)", Rule: storeclass.TestMintImport}: 1,
		// above the pin: every site of the key reports.
		{Path: "cmd/gc/callers.go", Func: "holder", Rule: storeclass.RawField}: 3,
		// below the pin: stale.
		{Path: "cmd/gc/callers.go", Func: "param", Rule: storeclass.RawParam}: 2,
		// a file this pass does not compile (another build-tag variant's, or
		// deleted): not this pass's to judge. The generator's check catches a
		// deleted file's row.
		{Path: "cmd/gc/gone.go", Func: "f", Rule: storeclass.Mint}: 1,
		// another package's row is not this package's business.
		{Path: "internal/other/x.go", Func: "f", Rule: storeclass.Mint}: 1,
	}
	got := findings(t, cfg)
	all := findings(t, config())
	silenced := []string{
		at("func two(", "raw-param"), at("func two(", "raw-param"),
		at("func (rigs) method(", "raw-param"), at("func (b *box[T]) put(", "raw-param"),
		at("var cityStores, rigStores", "raw-var"), at("var cityStores, rigStores", "raw-var"),
		at("func lit()", "raw-result"),
		at("\t\"example.com/m/internal/beads/beadstest\"", "test-mint-import"),
		// under its (stale) pin: the site is silent, the row reports.
		at("func param(", "raw-param"),
	}
	want := slices.Clone(all)
	for _, s := range silenced {
		i := slices.Index(want, s)
		if i < 0 {
			t.Fatalf("no unpinned finding %q to silence", s)
		}
		want = slices.Delete(want, i, i+1)
	}
	// the stale row reports at the package clause of the first linted file.
	want = append(want, "callers.go:1 baseline")
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("findings differ\n got: %s\nwant: %s", strings.Join(got, "\n      "), strings.Join(want, "\n      "))
	}
}

func at(prefix, rule string) string {
	return fmt.Sprintf("callers.go:%d %s", lineOf(callers, prefix), rule)
}

func TestStoreclassStaleMessages(t *testing.T) {
	cfg := config()
	cfg.Baseline = map[storeclass.Key]int{
		{Path: "cmd/gc/callers.go", Func: "param", Rule: storeclass.RawParam}: 2,
	}
	var stale []string
	for _, d := range analyzertest.RunGraph(t, storeclass.New(cfg), graph(callers)) {
		if strings.HasPrefix(d.Message, "storeclass[baseline]") {
			stale = append(stale, d.Message)
		}
	}
	if len(stale) != 1 || !strings.Contains(stale[0], "cmd/gc/callers.go\tparam\traw-param pins 2, the tree has 1") || !strings.Contains(stale[0], "only shrink") {
		t.Fatalf("stale findings = %q", stale)
	}
}

// TestStoreclassSealed: a sealed package reports every finding, whatever the
// baseline pins for it.
func TestStoreclassSealed(t *testing.T) {
	cfg := config()
	cfg.Sealed = []string{"cmd/gc"}
	cfg.Baseline = map[storeclass.Key]int{
		{Path: "cmd/gc/callers.go", Func: "two", Rule: storeclass.RawParam}: 2,
	}
	got := findings(t, cfg)
	want := findings(t, config())
	want = append(want, "callers.go:1 baseline")
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("findings differ\n got: %s\nwant: %s", strings.Join(got, "\n      "), strings.Join(want, "\n      "))
	}
}

func TestStoreclassStaleAllowlistFile(t *testing.T) {
	cfg := config()
	cfg.Allow = append(cfg.Allow, storeclass.Allow{Package: "cmd/gc", File: "gone.go", Reason: "a stale entry"})
	var msgs []string
	for _, d := range analyzertest.RunGraph(t, storeclass.New(cfg), graph(callers)) {
		if strings.Contains(d.Message, "gone.go") {
			msgs = append(msgs, d.Message)
		}
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0], "allowlisted file cmd/gc/gone.go names nothing") {
		t.Fatalf("stale allowlist findings = %q", msgs)
	}
}

// TestStoreclassOutsideModule: a package outside the module is not linted.
func TestStoreclassOutsideModule(t *testing.T) {
	cfg := config()
	cfg.Module = "example.com/other/"
	if got := analyzertest.RunGraph(t, storeclass.New(cfg), graph(callers)); len(got) != 0 {
		t.Fatalf("a package outside the module reported %v", got)
	}
}

// TestStoreclassCollect: the collector the baseline generator runs sees every
// finding, keyed as the baseline keys it, and reports nothing itself. A
// finding an Allow entry for its file exempts reaches it marked Allowed, for
// allowed.txt rather than the baseline: storage_boot.go's for every rule,
// infra_class_migrate.go's for the raw rules only.
func TestStoreclassCollect(t *testing.T) {
	var found []storeclass.Finding
	cfg := config()
	a := storeclass.NewCollector(cfg, func(f storeclass.Finding) { found = append(found, f) })
	if got := analyzertest.RunGraph(t, a, graph(callers)); len(got) != 0 {
		t.Fatalf("the collector reported %v", got)
	}
	counts, allowed := map[storeclass.Key]int{}, map[storeclass.Key]int{}
	for _, f := range found {
		if f.Allowed {
			allowed[f.Key]++
			continue
		}
		counts[f.Key]++
	}
	boot := storeclass.Key{Path: "cmd/gc/storage_boot.go", Func: "boot", Rule: storeclass.RawParam}
	migrate := storeclass.Key{Path: "cmd/gc/infra_class_migrate.go", Func: "migrate", Rule: storeclass.RawParam}
	for k, want := range map[storeclass.Key]int{
		{Path: "cmd/gc/callers.go", Func: "two", Rule: storeclass.RawParam}:             2,
		{Path: "cmd/gc/callers.go", Func: "holder", Rule: storeclass.RawField}:          4,
		{Path: "cmd/gc/callers.go", Func: "rigs.method", Rule: storeclass.RawParam}:     1,
		{Path: "cmd/gc/callers.go", Func: "box.put", Rule: storeclass.RawParam}:         1,
		{Path: "cmd/gc/callers.go", Func: "localType", Rule: storeclass.RawField}:       1,
		{Path: "cmd/gc/callers.go", Func: "(import)", Rule: storeclass.TestMintImport}:  1,
		{Path: "cmd/gc/infra_class_migrate.go", Func: "migrate", Rule: storeclass.Mint}: 1,
		boot:    0,
		migrate: 0,
	} {
		if counts[k] != want {
			t.Errorf("%v: %d for the baseline, want %d", k, counts[k], want)
		}
	}
	for k, want := range map[storeclass.Key]int{
		boot: 1,
		{Path: "cmd/gc/storage_boot.go", Func: "boot", Rule: storeclass.Mint}: 1,
		migrate: 1,
		{Path: "cmd/gc/infra_class_migrate.go", Func: "migrate", Rule: storeclass.Mint}: 0,
		{Path: "cmd/gc/callers.go", Func: "two", Rule: storeclass.RawParam}:             0,
	} {
		if allowed[k] != want {
			t.Errorf("%v: %d for allowed.txt, want %d", k, allowed[k], want)
		}
	}
}

func TestParseBaseline(t *testing.T) {
	rows, err := storeclass.ParseBaseline("# a comment\n\ncmd/gc/a.go\tf\traw-param\t2\ncmd/gc/a.go\tT.m\tmint\t1\n")
	if err != nil {
		t.Fatal(err)
	}
	if rows[storeclass.Key{Path: "cmd/gc/a.go", Func: "f", Rule: storeclass.RawParam}] != 2 || len(rows) != 2 {
		t.Fatalf("rows = %v", rows)
	}
	for _, bad := range []string{
		"cmd/gc/a.go\tf\traw-param\n",                                  // no count
		"cmd/gc/a.go\tf\tnot-a-rule\t1\n",                              // unknown rule
		"cmd/gc/a.go\tf\traw-param\t0\n",                               // a zero row
		"cmd/gc/a.go\tf\traw-param\t1\ncmd/gc/a.go\tf\traw-param\t1\n", // a duplicate
	} {
		if _, err := storeclass.ParseBaseline(bad); err == nil {
			t.Errorf("ParseBaseline(%q) accepted it", bad)
		}
	}
	text := storeclass.FormatBaseline(rows)
	again, err := storeclass.ParseBaseline(text)
	if err != nil || len(again) != 2 {
		t.Fatalf("round trip: %v %v", again, err)
	}
}

// TestStoreclassNolintIsAFinding: the baseline is the only exemption, so a
// //nolint:storeclass directive reports at its file's package clause, where
// it cannot suppress itself.
func TestStoreclassNolintIsAFinding(t *testing.T) {
	src := callers + "\nfunc hidden(s beads.Store) {} //nolint:storeclass,unparam // why\n"
	var msgs []string
	for _, d := range analyzertest.RunGraph(t, storeclass.New(config()), graph(src)) {
		if strings.HasPrefix(d.Message, "storeclass[nolint]") {
			msgs = append(msgs, fmt.Sprintf("%s:%d %s", d.File, d.Line, d.Message))
		}
	}
	if len(msgs) != 1 || !strings.HasPrefix(msgs[0], "callers.go:1 ") {
		t.Fatalf("nolint findings = %q", msgs)
	}
}

func TestLoadBaselineFromEnv(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/tools/nogo/analyzers/storeclass/baseline.txt"
	if err := os.MkdirAll(dir+"/tools/nogo/analyzers/storeclass", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("cmd/gc/a.go\tf\traw-param\t2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(storeclass.MainRepoDataEnv, dir+"/other.txt,"+path)
	rows, err := storeclass.LoadBaselineFromEnv()
	if err != nil || rows[storeclass.Key{Path: "cmd/gc/a.go", Func: "f", Rule: storeclass.RawParam}] != 2 {
		t.Fatalf("rows = %v, %v", rows, err)
	}
	t.Setenv(storeclass.MainRepoDataEnv, "")
	if _, err := storeclass.LoadBaselineFromEnv(); err == nil {
		t.Fatal("an unset baseline input loaded")
	}
}

// TestStoreclassLoadsTheBaselineOnce: with no Baseline, the analyzer loads
// it for module packages, and a load failure fails the pass loudly.
func TestStoreclassLoadsTheBaselineOnce(t *testing.T) {
	cfg := config()
	loads := 0
	cfg.LoadBaseline = func() (map[storeclass.Key]int, error) {
		loads++
		return map[storeclass.Key]int{{Path: "cmd/gc/callers.go", Func: "two", Rule: storeclass.RawParam}: 2}, nil
	}
	for _, d := range analyzertest.RunGraph(t, storeclass.New(cfg), graph(callers)) {
		if d.Line == lineOf(callers, "func two(") {
			t.Fatalf("a loaded pin did not silence %s", d.Message)
		}
	}
	if loads != 1 {
		t.Fatalf("loads = %d, want 1", loads)
	}
}

// mintImporterSrc is a production file under internal/beads that imports the
// test mint.
const mintImporterSrc = `package engine

import (
	"example.com/m/internal/beads"
	"example.com/m/internal/beads/beadstest" // test-mint-import
)

func open(s beads.Store) beads.WorkStore { return beadstest.Work(s) }

func mint(s beads.Store) beads.GraphStore { return beads.GraphStore{Store: s} }
`

// TestStoreclassTestMintStaysOutOfInternalBeads: Default's allowlist exempts
// internal/beads and the subpackages it names from every other rule, but a
// production file there may not import the test mint. The test mints may
// import each other.
func TestStoreclassTestMintStaysOutOfInternalBeads(t *testing.T) {
	if got, want := runDefault(t, "internal/beads/exec", "engine.go", mintImporterSrc), expected("engine.go", mintImporterSrc); !slices.Equal(got, want) {
		t.Errorf("a production importer of the test mint under internal/beads: findings %v, want %v", got, want)
	}
	splittest := strings.Replace(mintImporterSrc, "package engine", "package splittest", 1)
	if got := runDefault(t, "internal/beads/splittest", "splittest.go", splittest); len(got) != 0 {
		t.Errorf("a test mint importing the other: findings %v, want none", got)
	}
}

// newEngineSrc is a raw-store engine in a package of its own.
const newEngineSrc = `package engine

import "example.com/m/internal/beads"

func open(s beads.Store) beads.GraphStore { return beads.GraphStore{Store: s} } // raw-param mint
`

// TestStoreclassAllowsNamedPackagesOnly: an Allow entry exempts the package
// it names and none below it, so a package added under internal/beads is
// linted until the allowlist, a protected path, names it.
func TestStoreclassAllowsNamedPackagesOnly(t *testing.T) {
	if got := runDefault(t, "internal/beads/exec", "engine.go", newEngineSrc); len(got) != 0 {
		t.Fatalf("fixture: the engine reports in a package the allowlist names: %v", got)
	}
	want := expected("engine.go", newEngineSrc)
	sort.Strings(want)
	for _, pkg := range []string{"internal/beads/engine", "internal/storebinding/engine"} {
		if got := runDefault(t, pkg, "engine.go", newEngineSrc); !slices.Equal(got, want) {
			t.Errorf("%s, a package the allowlist does not name: findings %v, want %v", pkg, got, want)
		}
	}
}

// runDefault runs the analyzer with Default's allowlist over one file of
// package pkg, which may import beads and beadstest, and returns its findings
// as "file:line rule", sorted.
func runDefault(t *testing.T, pkg, file, src string) []string {
	t.Helper()
	cfg := config()
	cfg.Allow = storeclass.Default.Allow
	cfg.TestMintPackages = []string{module + "internal/beads/beadstest", module + "internal/beads/splittest"}
	g := graph(callers)[:2:2] // beads and beadstest
	g = append(g, analyzertest.Package{Path: module + pkg, Files: map[string]string{file: src}})
	var got []string
	for _, d := range analyzertest.RunGraph(t, storeclass.New(cfg), g) {
		m := ruleRE.FindStringSubmatch(d.Message)
		if m == nil {
			t.Fatalf("unexpected finding %s:%d %s", d.File, d.Line, d.Message)
		}
		got = append(got, fmt.Sprintf("%s:%d %s", d.File, d.Line, m[1]))
	}
	sort.Strings(got)
	return got
}
