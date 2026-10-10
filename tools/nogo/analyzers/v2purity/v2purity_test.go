package v2purity_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/internal/analyzertest"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/v2purity"
)

const pkg = "example.com/p"

// cfg stands local declarations in for the real forbidden targets, since
// analyzertest's packages import no standard library: example.com/os is
// the forbidden package, p.Now a forbidden function, p.Store a forbidden
// interface.
var cfg = v2purity.Config{
	Package:             pkg,
	Module:              "example.com/",
	ForbiddenPackages:   []string{"example.com/os"},
	ForbiddenFuncs:      []string{pkg + ".Now"},
	ForbiddenInterfaces: []string{pkg + ".Store"},
}

const osSrc = `package os

var Stderr int

var ErrGone error

func Getenv() int { return 0 }

type File struct{}

func (File) Write() int { return 0 }
`

const base = `package p

func Now() int { return 0 }

type Store interface{ Get() int }

type Other interface{ Get() int }
`

// messages runs c over p's src (after its package clause), with q's src
// as example.com/q when set, and returns each diagnostic of p as
// "file:line: message".
func messages(t *testing.T, c v2purity.Config, q, src string) []string {
	t.Helper()
	return messagesIn(t, false, c, q, src)
}

// messagesIn is messages under a source driver, or, with exportView, an
// export-data one (go vet, nogo).
func messagesIn(t *testing.T, exportView bool, c v2purity.Config, q, src string) []string {
	t.Helper()
	pkgs := []analyzertest.Package{{Path: "example.com/os", Files: map[string]string{"os.go": osSrc}}}
	if q != "" {
		pkgs = append(pkgs, analyzertest.Package{Path: "example.com/q", Files: map[string]string{"q.go": "package q\n\n" + q}})
	}
	pkgs = append(pkgs, analyzertest.Package{Path: pkg, Files: map[string]string{"base.go": base, "p.go": "package p\n\n" + src}})
	run := analyzertest.RunGraph
	if exportView {
		run = analyzertest.RunGraphExportView
	}
	var out []string
	for _, d := range run(t, v2purity.New(c), pkgs) {
		if d.File == "base.go" || d.File == "p.go" {
			out = append(out, d.File+":"+strconv.Itoa(d.Line)+": "+d.Message)
		}
	}
	return out
}

// check fails t unless got is exactly one diagnostic containing want, or,
// with want "", none.
func check(t *testing.T, name string, got []string, want string) {
	t.Helper()
	switch {
	case want == "" && len(got) != 0:
		t.Errorf("%s: diagnostics %q, want none", name, got)
	case want != "" && (len(got) != 1 || !strings.Contains(got[0], want)):
		t.Errorf("%s: diagnostics %q, want one containing %q", name, got, want)
	}
}

// Kills reach dropped at each kind of edge in the package, each with a case
// that must not report: a direct call, a helper, a method, an arm table, a
// //gc:pure variable, a forbidden package or interface, an interface of the
// package (every implementing type's method), a promoted or embedded
// interface method, a generic type's method, a variable assigned in init, a
// function field read (and not a data field), a //gc:pure field and
// //gc:pure-param parameter as roots, and another package's rules.
func TestPurityRules(t *testing.T) {
	impure := "\n\nfunc impure() int { return Now() }\n\nfunc clean() int { return 1 }\n"
	for _, c := range []struct {
		name, src string
		cfg       v2purity.Config
		want      string // "" is no finding
	}{
		{"direct call", "//gc:pure\nfunc decide() int { return Now() }\n", cfg, "decide reaches example.com/p.Now, but decide is //gc:pure (via decide)"},
		{"unmarked", "func decide() int { return Now() }\n", cfg, ""},
		{"through a helper", "//gc:pure\nfunc decide() int { return impure() }" + impure, cfg, "(via decide -> impure)"},
		{"clean helper", "//gc:pure\nfunc decide() int { return clean() }" + impure, cfg, ""},
		{"a method", "type h struct{}\n\nfunc (h) get() int { return Now() }\n\n//gc:pure\nfunc decide(x h) int { return x.get() }\n", cfg, "(via decide -> h.get)"},
		{"an arm table", "//gc:pure\nfunc decide() (n int) {\n\tfor _, a := range arms {\n\t\tn += a()\n\t}\n\treturn n\n}\n\nvar arms = []func() int{impure}" + impure, cfg, "(via decide -> arms -> impure)"},
		{"a pure variable", "//gc:pure\nvar sections = []func() int{func() int { return Now() }}\n", cfg, "but sections is //gc:pure"},
		{"an unmarked variable", "var sections = []func() int{func() int { return Now() }}\n", cfg, ""},
		{"a forbidden package", "//gc:pure\nfunc decide() int { return clean() }" + impure, v2purity.Config{Package: pkg, ForbiddenPackages: []string{pkg}}, "reaches example.com/p.clean"},
		{"a forbidden interface", "//gc:pure\nfunc decide(s Store) int { return s.Get() }\n", cfg, "reaches example.com/p.Store.Get"},
		{"an interface with no impure type", "//gc:pure\nfunc decide(s Other) int { return s.Get() }\n", cfg, ""},
		{"an interface of the package", "type getter interface{ get() int }\n\ntype wall struct{}\n\nfunc (wall) get() int { return Now() }\n\n//gc:pure\nfunc decide(g getter) int { return g.get() }\n", cfg, "wall.get reaches example.com/p.Now"},
		{"an interface no impure type implements", "type getter interface{ get() int }\n\ntype fixed struct{}\n\nfunc (fixed) get() int { return 1 }\n\n//gc:pure\nfunc decide(g getter) int { return g.get() }\n", cfg, ""},
		{"a promoted interface method", "type wrap struct{ Store }\n\n//gc:pure\nfunc decide(w wrap) int { return w.Get() }\n", cfg, "reaches example.com/p.Store.Get"},
		{"an embedding interface", "type ext interface {\n\tStore\n\tMore()\n}\n\n//gc:pure\nfunc decide(e ext) int { return e.Get() }\n", cfg, "reaches example.com/p.Store.Get"},
		{"a generic type's method", "type box[T any] struct{ v T }\n\nfunc (box[T]) get() int { return Now() }\n\n//gc:pure\nfunc decide(b box[int]) int { return b.get() }\n", cfg, "(via decide -> box.get)"},
		{"a variable assigned in init", "var hook func() int\n\nfunc init() { hook = impure }\n\n//gc:pure\nfunc decide() int { return hook() }" + impure, cfg, "(via decide -> hook -> impure)"},
		{"a variable assigned a clean function", "var hook func() int\n\nfunc init() { hook = clean }\n\n//gc:pure\nfunc decide() int { return hook() }" + impure, cfg, ""},
		{"a function field read", "type spec struct {\n\tclass int\n\trun   func() int\n}\n\nvar specs = map[int]spec{1: {class: 1, run: impure}}\n\n//gc:pure\nfunc decide() int { return specs[1].run() }" + impure, cfg, "(via decide -> run -> impure)"},
		{"a data field read", "type spec struct {\n\tclass int\n\trun   func() int\n}\n\nvar specs = map[int]spec{1: {class: 1, run: impure}}\n\n//gc:pure\nfunc decide() int { return specs[1].class }" + impure, cfg, ""},
		{"a field root", "type section struct {\n\t//gc:pure\n\tDecide func() int\n}\n\nvar secs = []section{{Decide: impure}}" + impure, cfg, "but section.Decide is //gc:pure"},
		{"an unmarked field", "type section struct {\n\tDecide func() int\n}\n\nvar secs = []section{{Decide: impure}}" + impure, cfg, ""},
		{"a pure parameter", "//gc:pure-param d\nfunc probed(d func() int) int { return 0 }\n\nvar x = probed(impure)" + impure, cfg, "but probed(d) is //gc:pure"},
		{"a promoted embedded interface implementing the call", "type getter interface{ Get() int }\n\ntype wrap struct{ Store }\n\n//gc:pure\nfunc decide(g getter) int { return g.Get() }\n", cfg, "reaches example.com/p.Store.Get"},
		{"a generic type implementing the call", "type getter interface{ get() int }\n\ntype box[T any] struct{ v T }\n\nfunc (box[T]) get() int { return Now() }\n\n//gc:pure\nfunc decide(g getter) int { return g.get() }\n", cfg, "box.get reaches example.com/p.Now"},
		{"a registry filled by index", "var reg = map[int]func() int{}\n\nfunc init() { reg[1] = impure }\n\n//gc:pure\nfunc decide() int { return reg[1]() }" + impure, cfg, "(via decide -> reg -> impure)"},
		{"a registry filled by append", "var reg []func() int\n\nfunc init() { reg = append(reg, impure) }\n\n//gc:pure\nfunc decide() int { return reg[0]() }" + impure, cfg, "(via decide -> reg -> impure)"},
		{"a pure field given a value it cannot follow", "type section struct {\n\t//gc:pure\n\tDecide func() int\n}\n\nvar d = clean\n\nvar secs = []section{{Decide: d}}" + impure, cfg, "cannot follow this value into //gc:pure field Decide"},
		{"a field store by assignment", "type box struct{ f func() int }\n\nvar b box\n\nfunc init() { b.f = impure }\n\n//gc:pure\nfunc decide() int { return b.f() }" + impure, cfg, "(via decide -> f -> impure)"},
		{"a field registry by append", "type reg struct{ fs []func() int }\n\nvar r reg\n\nfunc init() { r.fs = append(r.fs, impure) }\n\n//gc:pure\nfunc decide() int { return r.fs[0]() }" + impure, cfg, "(via decide -> fs -> impure)"},
		{"a field registry by index", "type reg struct{ m map[int]func() int }\n\nvar r = reg{m: map[int]func() int{}}\n\nfunc init() { r.m[1] = impure }\n\n//gc:pure\nfunc decide() int { return r.m[1]() }" + impure, cfg, "(via decide -> m -> impure)"},
		{"an interface-typed field holding a function", "type spec struct{ f any }\n\nvar s = spec{f: impure}\n\n//gc:pure\nfunc decide() int { return s.f.(func() int)() }" + impure, cfg, "(via decide -> f -> impure)"},
		{"a look-alike struct converted into a pure field's", "type section struct {\n\t//gc:pure\n\tDecide func() int\n}\n\ntype twin struct{ Decide func() int }\n\nvar secs = []section{section(twin{Decide: impure})}" + impure, cfg, "but section.Decide is //gc:pure"},
		{"a pure parameter with explicit type arguments", "//gc:pure-param d\nfunc probed[T any](d func() T) int { return 0 }\n\nvar x = probed[int](impure)" + impure, cfg, "but probed(d) is //gc:pure"},
		{"a pure field given nil", "type section struct {\n\t//gc:pure\n\tDecide func() int\n}\n\nvar secs = []section{{Decide: nil}}" + impure, cfg, ""},
		{"another package", "//gc:pure\nfunc decide() int { return Now() }\n", v2purity.Config{Package: "example.com/r", ForbiddenFuncs: cfg.ForbiddenFuncs}, ""},
		{"a method of the forbidden function's name", "type clock struct{}\n\nfunc (clock) Now() int { return 0 }\n\n//gc:pure\nfunc decide(c clock) int { return c.Now() }\n", cfg, ""},
	} {
		check(t, c.name, messages(t, c.cfg, "", c.src), c.want)
	}
}

// Kills reach stopped at the package boundary: a Module package's function
// carries a Reaches fact, through its own helpers, to a caller in the
// rules' package, and so do its types' methods called directly or through
// an interface, and the functions stored in its fields; a forbidden
// package's variable is a target, an error sentinel not; a package outside
// the Module exports nothing.
func TestFactsCrossPackages(t *testing.T) {
	q := `import "example.com/os"

func Helper() int { return inner() }

func inner() int { return os.Getenv() }

func Clean() int { return 1 }

type MemStore struct{}

func (MemStore) Get() int { return os.Getenv() }

type Observer interface{ Observe() int }

type Tmux struct{}

func (Tmux) Observe() int { return os.Getenv() }

type Spec struct{ Run func() int }

var Specs = []Spec{{Run: Helper}}

var Hook = Helper

type Box struct{ F func() int }

func Run(b Box) int { return b.F() }

type Getter interface{ Fetch() int }

type Wrapped struct{ Getter }

type osGetter struct{}

func (osGetter) Fetch() int { return os.Getenv() }

var _ Getter = osGetter{}
`
	imp := "import (\n\t\"example.com/os\"\n\t\"example.com/q\"\n)\n\nvar _ = os.Getenv\n\nvar _ = q.Clean\n\n"
	outside, qOnly := cfg, cfg
	outside.Module, qOnly.Module = "example.org/", "example.com/q"
	for _, c := range []struct {
		name, src string
		cfg       v2purity.Config
		want      string
	}{
		{"an imported function", "//gc:pure\nfunc decide() int { return q.Helper() }\n", cfg, "reaches example.com/q.Helper, which reaches example.com/os.Getenv (via Helper -> inner)"},
		{"a clean imported function", "//gc:pure\nfunc decide() int { return q.Clean() }\n", cfg, ""},
		{"a concrete type's method", "//gc:pure\nfunc decide(m q.MemStore) int { return m.Get() }\n", cfg, "reaches (example.com/q.MemStore).Get, which reaches example.com/os.Getenv"},
		{"an interface implemented in another package", "//gc:pure\nfunc decide(o q.Observer) int { return o.Observe() }\n", cfg, "reaches (example.com/q.Tmux).Observe, which reaches"},
		{"an imported field read", "//gc:pure\nfunc decide(s q.Spec) int { return s.Run() }\n", cfg, "reaches example.com/q.Run (a field), which reaches"},
		{"a forbidden package's variable", "//gc:pure\nfunc decide() int { return os.Stderr }\n", cfg, "reaches example.com/os.Stderr"},
		{"an error sentinel", "//gc:pure\nfunc decide() error { return os.ErrGone }\n", cfg, ""},
		{"a package outside the Module", "//gc:pure\nfunc decide() int { return q.Helper() }\n", outside, ""},
		{"an imported function variable", "//gc:pure\nfunc decide() int { return q.Hook() }\n", cfg, "reaches example.com/q.Hook (a variable), which reaches example.com/os.Getenv"},
		{"a store into another package's field", "//gc:pure\nfunc decide() int { return q.Run(q.Box{F: impure}) }\n\nfunc impure() int { return os.Getenv() }\n", cfg, "impure reaches example.com/os.Getenv"},
		{"a promoted embedded interface in another package", "type fetcher interface{ Fetch() int }\n\n//gc:pure\nfunc decide(f fetcher) int { return f.Fetch() }\n", cfg, "reaches (example.com/q.Wrapped).Fetch, which reaches example.com/os.Getenv (via osGetter.Fetch)"},
		{"an interface only a type outside the Module implements impurely", "type writer interface{ Write() int }\n\n//gc:pure\nfunc decide(w writer) int { return w.Write() }\n", qOnly, ""},
	} {
		check(t, c.name, messages(t, c.cfg, q, imp+c.src), c.want)
	}
}

// Kills the baseline rules dropped: an exempt function's reach suppressed
// and not the rest; a stale entry (reached from no root, reaching nothing,
// reaching only through another entry, naming nothing) and a malformed one
// (no class, a dead one with no line or no Via) reported; an imported
// function exempt; a dead entry checked against its Via.
func TestExemptionRules(t *testing.T) {
	src := "//gc:pure\nfunc decide() int { return helper() + other() + via() + clean() }\n\nfunc helper() int { return Now() }\n\nfunc other() int { return Now() }\n\nfunc via() int { return deep() }\n\nfunc deep() int { return Now() }\n\nfunc unreached() int { return Now() }\n\nfunc clean() int { return 1 }\n"
	with := func(e map[string]v2purity.Exemption) v2purity.Config {
		c := cfg
		c.Exempt = map[string]v2purity.Exemption{pkg + ".deep": live}
		for k, v := range e {
			c.Exempt[k] = v
		}
		return c
	}
	dead := v2purity.Exemption{Class: "dead", Bead: "b-1", Reason: "dead: nil at x.go:1", Via: []string{pkg + ".via"}}
	for _, c := range []struct {
		name string
		cfg  v2purity.Config
		want []string
	}{
		{"no baseline", cfg, []string{"helper reaches", "other reaches", "deep reaches"}},
		{"helper exempt", with(map[string]v2purity.Exemption{pkg + ".helper": live}), []string{"other reaches"}},
		{"unreached entry", with(map[string]v2purity.Exemption{pkg + ".helper": live, pkg + ".other": live, pkg + ".unreached": live}), []string{"unreached is reached from no //gc:pure root"}},
		{"entry reaching nothing", with(map[string]v2purity.Exemption{pkg + ".helper": live, pkg + ".other": live, pkg + ".clean": live}), []string{"clean holds no forbidden reference"}},
		{"entry reaching only through another", with(map[string]v2purity.Exemption{pkg + ".helper": live, pkg + ".other": live, pkg + ".via": live}), []string{"via holds no forbidden reference", "deep is reached from no //gc:pure root"}},
		{"entry naming nothing", with(map[string]v2purity.Exemption{pkg + ".helper": live, pkg + ".other": live, pkg + ".gone": live}), []string{"gone names no function"}},
		{"no class", with(map[string]v2purity.Exemption{pkg + ".helper": {Bead: "b", Reason: "r"}, pkg + ".other": live}), []string{"helper is malformed"}},
		{"dead citing no line", with(map[string]v2purity.Exemption{pkg + ".helper": {Class: "dead", Bead: "b", Reason: "nil", Via: []string{pkg + ".decide"}}, pkg + ".other": live}), []string{"helper is malformed"}},
		{"dead with no Via", with(map[string]v2purity.Exemption{pkg + ".helper": {Class: "dead", Bead: "b", Reason: "nil at x.go:1"}, pkg + ".other": live}), []string{"helper is malformed"}},
		{"an entry reaching through a helper", withOnly(map[string]v2purity.Exemption{pkg + ".helper": live, pkg + ".other": live, pkg + ".via": live}), nil},
		{"live citing no line", with(map[string]v2purity.Exemption{pkg + ".helper": {Class: "live", Bead: "b", Reason: "reads"}, pkg + ".other": live}), nil},
		{"dead behind its Via", with(map[string]v2purity.Exemption{pkg + ".helper": live, pkg + ".other": live, pkg + ".deep": dead}), nil},
		{"dead reached around its Via", with(map[string]v2purity.Exemption{pkg + ".helper": dead, pkg + ".other": live}), []string{"helper is dead through example.com/p.via, but a //gc:pure root reaches it around them"}},
		{"Via naming nothing", with(map[string]v2purity.Exemption{pkg + ".helper": {Class: "dead", Bead: "b", Reason: "nil at x.go:1", Via: []string{pkg + ".gone"}}, pkg + ".other": live}), []string{"Via example.com/p.gone names no function"}},
	} {
		got := messages(t, c.cfg, "", src)
		if len(got) != len(c.want) {
			t.Errorf("%s: diagnostics %q, want %d containing %q", c.name, got, len(c.want), c.want)
			continue
		}
		for i, w := range c.want {
			if !strings.Contains(got[i], w) {
				t.Errorf("%s: diagnostic %q, want %q", c.name, got[i], w)
			}
		}
	}
	q := "import \"example.com/os\"\n\nfunc Helper() int { return os.Getenv() }\n\nfunc Clean() int { return 1 }\n"
	imp := "import \"example.com/q\"\n\n//gc:pure\nfunc decide() int { return q.Helper() + q.Clean() }\n"
	check(t, "an imported function exempt, well formed", messages(t, withOnly(map[string]v2purity.Exemption{"example.com/q.Helper": live}), q, imp), "")
	check(t, "an imported entry reaching nothing", messages(t, withOnly(map[string]v2purity.Exemption{"example.com/q.Helper": live, "example.com/q.Clean": live}), q, imp), "example.com/q.Clean holds no forbidden reference")
	clock := "import \"example.com/os\"\n\ntype Clock interface{ Now() int }\n\ntype wall struct{}\n\nfunc (wall) Now() int { return os.Getenv() }\n"
	check(t, "another package's implementer exempt", messages(t, withOnly(map[string]v2purity.Exemption{"(example.com/q.wall).Now": live}), clock, "import \"example.com/q\"\n\n//gc:pure\nfunc decide(c q.Clock) int { return c.Now() }\n"), "")
	check(t, "an imported entry reached by no root", messages(t, withOnly(map[string]v2purity.Exemption{"example.com/q.Helper": live, "example.com/q.Other": live}), q, imp), "example.com/q.Other names no function a //gc:pure root reaches")
}

var live = v2purity.Exemption{Class: "live", Bead: "b-1", Reason: "live: reads"}

func withOnly(e map[string]v2purity.Exemption) v2purity.Config {
	c := cfg
	c.Exempt = e
	return c
}

// Kills the roots unpinned: with Roots set, a //gc:pure mark added or
// removed fails.
func TestRootsPinned(t *testing.T) {
	src := "//gc:pure\nfunc decide() int { return 1 }\n"
	pinned := cfg
	pinned.Roots = []string{pkg + ".decide"}
	check(t, "pinned", messages(t, pinned, "", src), "")
	check(t, "a root added", messages(t, pinned, "", src+"\n//gc:pure\nfunc extra() int { return 1 }\n"), "v2purity root example.com/p.extra is not pinned")
	check(t, "a root removed", messages(t, pinned, "", "func decide() int { return 1 }\n"), "pinned v2purity root example.com/p.decide is not marked //gc:pure")
}

// Kills the C9 gate dropped: once the cutover constant is true a live
// exemption fails, a dead one does not, and before cutover neither does;
// a missing constant fails.
func TestCutoverGate(t *testing.T) {
	src := "//gc:pure\nfunc decide() int { return helper() }\n\nfunc helper() int { return Now() }\n"
	for _, c := range []struct {
		name, cutover, class, want string
	}{
		{"live at cutover", "const effectsReal = true\n\n", "live", "effectsReal is true, but 1 v2purity exemptions are live"},
		{"dead at cutover", "const effectsReal = true\n\n", "dead", ""},
		{"live before cutover", "const effectsReal = false\n\n", "live", ""},
		{"no cutover constant", "", "live", "v2purity cutover constant effectsReal is not a bool constant"},
		{"a cutover that is not a bool", "const effectsReal = 1\n\n", "live", "is not a bool constant"},
	} {
		k := cfg
		k.Cutover = "effectsReal"
		k.Exempt = map[string]v2purity.Exemption{pkg + ".helper": {Class: c.class, Bead: "b", Reason: "r at x.go:1", Via: []string{pkg + ".decide"}}}
		if c.class == "dead" {
			k.Exempt[pkg+".helper"] = v2purity.Exemption{Class: "dead", Bead: "b", Reason: "r at x.go:1", Via: []string{pkg + ".helper2"}}
			src2 := "//gc:pure\nfunc decide() int { return helper2() }\n\nfunc helper2() int { return helper() }\n\nfunc helper() int { return Now() }\n"
			check(t, c.name, messages(t, k, "", c.cutover+src2), c.want)
			continue
		}
		check(t, c.name, messages(t, k, "", c.cutover+src), c.want)
	}
}

// baselineNames is the reviewed baseline: Default.Exempt may only drop
// names from it, and only its live names may be live (the coordinator's
// ruling). Adding a name, or making one live, means raising it in review.
var baselineNames = map[string]string{
	"(*github.com/gastownhall/gascity/cmd/gc.SessionReconcilerTraceCycle).RecordDecision":   "dead",
	"(*github.com/gastownhall/gascity/cmd/gc.controlDispatcherRouteRepair).write":           "dead",
	"github.com/gastownhall/gascity/cmd/gc.normalizeNonExpandingPoolSessionInfo":            "dead",
	"github.com/gastownhall/gascity/cmd/gc.recordDeferredNonExpandingPoolAliasConflictInfo": "dead",
	"github.com/gastownhall/gascity/cmd/gc.workerSessionTargetLastActivityWithConfig":       "dead",
	"github.com/gastownhall/gascity/cmd/gc.verifyPoolTriggerWorktree":                       "dead",
	"(*github.com/gastownhall/gascity/cmd/gc.sessionBeadSnapshot).OpenInfos":                "live",
	"(*github.com/gastownhall/gascity/cmd/gc.sessionBeadSnapshot).LoadError":                "live",
	"(*github.com/gastownhall/gascity/cmd/gc.sessionBeadSnapshot).FindInfoByID":             "live",
	"github.com/gastownhall/gascity/cmd/gc.applyTemplateOverridesToConfigInfo":              "live",
	"github.com/gastownhall/gascity/cmd/gc.templateParamsToConfigWithDelivery":              "live",
	"github.com/gastownhall/gascity/cmd/gc.recordNewDemandCapTrace":                         "live",
	"github.com/gastownhall/gascity/cmd/gc.reserveNestedCapFloors":                          "live",
	"github.com/gastownhall/gascity/internal/session.ProjectLifecycle":                      "live",
	"(*github.com/gastownhall/gascity/internal/poolplan.CreateBudget).TryClaim":             "live",
	"(*github.com/gastownhall/gascity/internal/poolplan.CreateBudget).ConfigureFairShare":   "live",
	"(*github.com/gastownhall/gascity/internal/config.DaemonConfig).PatrolIntervalDuration": "live",
	"github.com/gastownhall/gascity/internal/config.NamedSessionRuntimeName":                "live",
	"github.com/gastownhall/gascity/internal/session.FindNamedSessionSpec":                  "live",
	"github.com/gastownhall/gascity/internal/workdir.SessionQualifiedName":                  "live",
	"github.com/gastownhall/gascity/internal/workdir.ConfiguredRigName":                     "live",
	"github.com/gastownhall/gascity/internal/workdir.ResolveTmuxAlias":                      "live",
	"github.com/gastownhall/gascity/internal/workdir.ResolveWorkDirPathStrict":              "live",
	"github.com/gastownhall/gascity/cmd/gc.resolveConfiguredWorkDirPath":                    "dead",
}

func TestDefaultBaselineOnlyShrinks(t *testing.T) {
	for name, e := range v2purity.Default.Exempt {
		class, ok := baselineNames[name]
		switch {
		case !ok:
			t.Errorf("%s: not in the reviewed baseline; the baseline only shrinks", name)
		case e.Class == "live" && class != "live":
			t.Errorf("%s: live, but reviewed %s", name, class)
		}
		if e.Class != "live" && e.Class != "dead" || e.Bead == "" || e.Reason == "" || e.Class == "dead" && (!strings.Contains(e.Reason, ".go:") || len(e.Via) == 0) {
			t.Errorf("%s: malformed %+v", name, e)
		}
	}
	if v2purity.Default.Cutover != "v2EffectsReal" {
		t.Fatalf("cutover constant %q, want v2EffectsReal", v2purity.Default.Cutover)
	}
	if !slices.Contains(v2purity.Default.Roots, "section.Decide") || len(v2purity.Default.Roots) < 9 {
		t.Fatalf("roots %q, want the nine pinned ones", v2purity.Default.Roots)
	}
}

// Kills an interface call whose verdict depends on the driver (B-b review
// 2): an unexported I/O type of another Module package implementing the
// interface is found under a source driver and under an export-data one
// (go vet, nogo), which hides unexported types: through the package's
// Methods fact, not its scope. A method of the same name and another
// signature does not implement it.
func TestFactsUnderEveryDriver(t *testing.T) {
	q := "import \"example.com/os\"\n\ntype Clock interface{ Now() int }\n\ntype skew struct{}\n\nfunc (skew) Now(int) int { return os.Getenv() }\n\ntype wall struct{}\n\nfunc (wall) Now() int { return os.Getenv() }\n\nfunc Default() Clock { return wall{} }\n"
	p := "import \"example.com/q\"\n\n//gc:pure\nfunc decide(c q.Clock) int { return c.Now() }\n"
	for _, exportView := range []bool{false, true} {
		check(t, "export view "+strconv.FormatBool(exportView), messagesIn(t, exportView, cfg, q, p), "reaches (example.com/q.wall).Now, which reaches example.com/os.Getenv")
	}
}

// Pins fmt's printers, which write os.Stdout, among the forbidden functions.
func TestDefaultForbidsTheStdoutPrinters(t *testing.T) {
	for _, f := range []string{"fmt.Print", "fmt.Printf", "fmt.Println"} {
		if !slices.Contains(v2purity.Default.ForbiddenFuncs, f) {
			t.Errorf("%s is not forbidden", f)
		}
	}
}

// Kills a forbidden interface found only where the driver shows it: under
// an export-data driver, a package that reaches the interface only through
// another package's embedding does not see it among its imports, so the
// method is matched in its declaring package.
func TestForbiddenInterfaceUnderAnExportView(t *testing.T) {
	c := cfg
	c.ForbiddenInterfaces = []string{"example.com/q.Store"}
	pkgs := []analyzertest.Package{
		{Path: "example.com/q", Files: map[string]string{"q.go": "package q\n\ntype Store interface{ Get() int }\n"}},
		{Path: "example.com/r", Files: map[string]string{"r.go": "package r\n\nimport \"example.com/q\"\n\ntype Holder struct{ q.Store }\n"}},
		{Path: pkg, Files: map[string]string{"base.go": base, "p.go": "package p\n\nimport \"example.com/r\"\n\n//gc:pure\nfunc decide(h r.Holder) int { return h.Get() }\n"}},
	}
	var got []string
	for _, d := range analyzertest.RunGraphExportView(t, v2purity.New(c), pkgs) {
		got = append(got, d.Message)
	}
	check(t, "export view", got, "reaches example.com/q.Store.Get")
}

// Kills a summary that does not travel past a direct import (B-b review 3):
// under an export-data driver a package sees only its direct imports'
// facts, so lib2 carries lib3's unexported I/O type in its Methods fact,
// and the rules' package, importing lib2 only, finds it, as a source driver
// does.
func TestFactsTravelPastADirectImport(t *testing.T) {
	lib3 := "package lib3\n\nimport \"example.com/os\"\n\ntype Clock interface{ Now() int }\n\ntype wall struct{}\n\nfunc (wall) Now() int { return os.Getenv() }\n\nfunc Default() Clock { return wall{} }\n"
	lib2 := "package lib2\n\nimport \"example.com/lib3\"\n\ntype Clock = lib3.Clock\n\nfunc Default() Clock { return lib3.Default() }\n"
	p := "package p\n\nimport \"example.com/lib2\"\n\n//gc:pure\nfunc decide(c lib2.Clock) int { return c.Now() }\n"
	pkgs := []analyzertest.Package{
		{Path: "example.com/os", Files: map[string]string{"os.go": osSrc}},
		{Path: "example.com/lib3", Files: map[string]string{"lib3.go": lib3}},
		{Path: "example.com/lib2", Files: map[string]string{"lib2.go": lib2}},
		{Path: pkg, Files: map[string]string{"base.go": base, "p.go": p}},
	}
	for name, run := range map[string]func(*testing.T, *analysis.Analyzer, []analyzertest.Package) []analyzertest.Diagnostic{
		"source": analyzertest.RunGraph, "export view": analyzertest.RunGraphExportView,
	} {
		var got []string
		for _, d := range run(t, v2purity.New(cfg), pkgs) {
			if d.File == "p.go" {
				got = append(got, d.Message)
			}
		}
		check(t, name, got, "reaches (example.com/lib3.wall).Now, which reaches example.com/os.Getenv")
	}
}
