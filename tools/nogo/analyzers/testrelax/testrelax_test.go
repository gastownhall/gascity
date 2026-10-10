package testrelax_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/internal/analyzertest"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/testrelax"
)

const pkg = "example.com/p"

// src is production code: the guard setter, the variable it holds, a seam
// setter that is no guard, helpers that reach the guard directly, through a
// chain, or as a value, a reviewed writer of the variable and an unreviewed
// one.
const src = `package p

var root = "/proc"

func AllowForTest() (restore func()) { prev := root; root = "/fake"; return func() { root = prev } }

func SetClockForTest() (restore func()) { return func() {} }

type T struct{}

func (T) Relax() {}

func helper()      { _ = AllowForTest() }
func chain()       { helper() }
func value() func() (restore func()) { return AllowForTest }

func Setenv(key, value string) error { return nil }

func lazyRoot() {
	if root == "" {
		root = "/proc"
	}
}

func Override() { root = "/elsewhere" } // P1

func init() { _ = SetClockForTest(); lazyRoot() }
`

// testSrc is the package's test file.
const testSrc = `package p

type M struct{}

func (M) Run() int { return 0 }

const breakGlass = "GC_BREAK_GLASS"

func init() { _ = AllowForTest() } // L1

func init() { chain() } // L2

func TestMain(m M) { _ = value(); _ = m.Run() } // L3

func TestMainLike(m M) { _ = AllowForTest(); _ = m.Run() }

var restore = AllowForTest() // L4

var relax = T{}.Relax // L5

func init() { root = "/other" } // L6

func init() { _ = Setenv(breakGlass, "1") }

func init() { Override() } // L8

func TestScoped() {
	restore := AllowForTest()
	defer restore()
}

func TestWritesTheVariable() { root = "/fake" } // L7
`

// cfg lists src's guard, variable and writer.
var cfg = testrelax.Config{
	Guards: map[string]string{
		pkg + ".AllowForTest":   "lets a test skip the check",
		"(" + pkg + ".T).Relax": "relaxes the method's check",
	},
	Vars:    map[string]string{pkg + ".root": "is the guarded root"},
	Writers: map[string]string{pkg + ".lazyRoot": "builds the default"},
}

func run(t *testing.T, cfg testrelax.Config, files map[string]string) []string {
	t.Helper()
	var got []string
	for _, d := range analyzertest.Run(t, testrelax.New(cfg), pkg, files) {
		got = append(got, fmt.Sprintf("%s:%d %s", d.File, d.Line, d.Message))
	}
	return got
}

// want checks got has, for each marker, the finding on the marker's line, and
// nothing else.
func want(t *testing.T, got []string, files map[string]string, want map[string]string) {
	t.Helper()
	for marker, msg := range want {
		prefix := "" // the unmarked finding: anywhere
		if marker != "" {
			file, line := markerLine(files, marker)
			prefix = fmt.Sprintf("%s:%d ", file, line)
		}
		if !contains(got, prefix, msg) {
			t.Errorf("no finding %q at %q", msg, prefix)
		}
	}
	if len(got) != len(want) {
		t.Errorf("findings = %d, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
}

func TestTestRelax(t *testing.T) {
	files := map[string]string{"p.go": src, "p_test.go": testSrc}
	want(t, run(t, cfg, files), files, map[string]string{
		"L1": "init relaxes example.com/p.AllowForTest for every test in the package: it lets a test skip the check",
		"L2": "init relaxes example.com/p.AllowForTest for every test in the package (via chain)",
		"L3": "TestMain relaxes example.com/p.AllowForTest for every test in the package (via value)",
		"L4": "a package-level initializer relaxes example.com/p.AllowForTest",
		"L5": "a package-level initializer relaxes (example.com/p.T).Relax",
		"L6": "init relaxes example.com/p.root for every test in the package: it is the guarded root",
		"L7": "TestWritesTheVariable writes example.com/p.root, which is the guarded root; a test writes it through its setter",
		"L8": "init relaxes example.com/p.root for every test in the package (via Override)",
		"P1": "Override writes example.com/p.root, which is the guarded root; a test writes it through its setter, scoped to t, and production code only in a writer testrelax lists",
	})
}

// TestTestRelaxIgnoresUnlistedSetters: a seam setter off the reviewed list
// may be called from init, and an unlisted variable written anywhere.
func TestTestRelaxIgnoresUnlistedSetters(t *testing.T) {
	if got := run(t, testrelax.Config{}, map[string]string{"p.go": src, "p_test.go": testSrc}); len(got) != 0 {
		t.Fatalf("with no guards listed, findings = %v", got)
	}
}

// TestTestRelaxWriteForms: taking a guarded variable's address, calling its
// pointer-receiver method, or writing under it (a field, an element) is a
// write; reading it, or calling a value-receiver method or a method through
// a pointer it holds, is not.
func TestTestRelaxWriteForms(t *testing.T) {
	const src = `package p

type Flag struct{ on bool }

func (f *Flag) Store(on bool) { f.on = on }
func (f Flag) On() bool       { return f.on }

type Box struct{ n int }

func (b *Box) Bump() { b.n++ }

var (
	flag   Flag
	limits = map[string]int{}
	cfg    struct{ n int }
	box    = &Box{}
	root   = "/proc"
)

func swap(p *string, v string) { *p = v }
`
	const testSrc = `package p

func TestAddress() { p := &root; *p = "/fake" } // W1

func TestSwap() { swap(&root, "/fake") } // W2

func TestStore() { flag.Store(true) } // W3

func TestMethodValue() { store := flag.Store; store(true) } // W4

func TestField() { cfg.n = 2 } // W5

func TestElement() { limits["k"] = 1 } // W6

func TestReads() {
	_ = flag.On()
	_ = root
	box.Bump()
}
`
	vars := map[string]string{}
	for _, v := range []string{"flag", "limits", "cfg", "box", "root"} {
		vars[pkg+"."+v] = "is guarded"
	}
	files := map[string]string{"p.go": src, "p_test.go": testSrc}
	want(t, run(t, testrelax.Config{Vars: vars}, files), files, map[string]string{
		"W1": "TestAddress writes example.com/p.root",
		"W2": "TestSwap writes example.com/p.root",
		"W3": "TestStore writes example.com/p.flag",
		"W4": "TestMethodValue writes example.com/p.flag",
		"W5": "TestField writes example.com/p.cfg",
		"W6": "TestElement writes example.com/p.limits",
	})
}

// TestTestRelaxAllowlist: an allowlisted relaxation is kept at its one site
// only: another init, in another file, and a test that writes the variable
// are reported, and an entry no site uses any more is.
func TestTestRelaxAllowlist(t *testing.T) {
	const kept = `package p

func init() { root = "/kept" }
`
	const other = `package p

func init() { root = "/other" } // A1

func TestLeaks() { root = "/leak" } // A2
`
	files := map[string]string{"p.go": src, "kept_test.go": kept, "other_test.go": other}
	got := run(t, testrelax.Config{
		Vars:    cfg.Vars,
		Writers: cfg.Writers,
		Allowed: map[testrelax.Allowance]string{
			{Pkg: pkg, Site: "kept_test.go:init", Guard: pkg + ".root"}: "reviewed",
			{Pkg: pkg, Site: "gone_test.go:init", Guard: pkg + ".root"}: "stale",
		},
	}, files)
	got = dropPrefixed(got, "p.go:") // AllowForTest and Override write root in production: not this test's subject
	want(t, got, files, map[string]string{
		"A1": "init relaxes example.com/p.root for every test in the package",
		"A2": "TestLeaks writes example.com/p.root",
		"":   "allowlisted gone_test.go:init example.com/p.root is not relaxed there any more: drop it",
	})
}

// TestTestRelaxEnv: setting a listed break-glass variable through a constant
// key is reported in init, in a test, and in a test file's helper; t.Setenv,
// an unlisted key, and production code are not; a test file that takes
// os.Setenv as a value is.
func TestTestRelaxEnv(t *testing.T) {
	const osSrc = `package os

func Setenv(key, value string) error { return nil }
`
	const prodSrc = `package q

import "os"

func Configure() { _ = os.Setenv("GC_BREAK_GLASS", "1") }
`
	const testSrc = `package q

import "os"

type T struct{}

func (T) Setenv(key, value string) {}

const breakGlass = "GC_BREAK_GLASS"

func init() { _ = os.Setenv(breakGlass, "1") } // E1

func init() { _ = os.Setenv("GC_OTHER", "1") }

func TestLeaks(t T) { _ = os.Setenv(breakGlass, "1") } // E2

func setGlass() { _ = os.Setenv("GC_BREAK_GLASS", "") } // E3

func TestScoped(t T) { t.Setenv(breakGlass, "1") }

func TestValue() { set := os.Setenv; _ = set("GC_OTHER", "1") } // E4
`
	diags := analyzertest.RunGraph(t, testrelax.New(testrelax.Config{Env: map[string]string{"GC_BREAK_GLASS": "lets a caller past the glass"}}), []analyzertest.Package{
		{Path: "os", Files: map[string]string{"os.go": osSrc}},
		{Path: "example.com/q", Files: map[string]string{"q.go": prodSrc, "q_test.go": testSrc}},
	})
	var got []string
	for _, d := range diags {
		got = append(got, fmt.Sprintf("%s:%d %s", d.File, d.Line, d.Message))
	}
	files := map[string]string{"q_test.go": testSrc}
	want(t, got, files, map[string]string{
		"E1": "init relaxes GC_BREAK_GLASS for every test in the package: it lets a caller past the glass",
		"E2": "TestLeaks sets GC_BREAK_GLASS, which lets a caller past the glass, past the test that set it; set it with t.Setenv",
		"E3": "setGlass sets GC_BREAK_GLASS",
		"E4": "a test takes os.Setenv as a value, so the keys it sets go unchecked",
	})
}

// TestTestRelaxShippedEnv: the shipped list has the rollout fences' env
// overrides, which turn a write fence off.
func TestTestRelaxShippedEnv(t *testing.T) {
	const osSrc = `package os

func Setenv(key, value string) error { return nil }
`
	const testSrc = `package q

import "os"

func init() { _ = os.Setenv("GC_BEADS_CONDITIONAL_WRITES", "off") } // S1

func init() { _ = os.Setenv("GC_BEADS_GUARDED_RELEASE", "off") } // S2
`
	diags := analyzertest.RunGraph(t, testrelax.Analyzer, []analyzertest.Package{
		{Path: "os", Files: map[string]string{"os.go": osSrc}},
		{Path: "example.com/q", Files: map[string]string{"q_test.go": testSrc}},
	})
	var got []string
	for _, d := range diags {
		got = append(got, fmt.Sprintf("%s:%d %s", d.File, d.Line, d.Message))
	}
	files := map[string]string{"q_test.go": testSrc}
	want(t, got, files, map[string]string{
		"S1": "init relaxes GC_BEADS_CONDITIONAL_WRITES",
		"S2": "init relaxes GC_BEADS_GUARDED_RELEASE",
	})
}

// TestTestRelaxAcrossPackages: a helper in another package that reaches a
// guard, by a call or by a write of a guarded variable, carries it to an init
// that calls it, as an export-data driver (nogo, go vet) shows the helper's
// package.
func TestTestRelaxAcrossPackages(t *testing.T) {
	const guardSrc = `package guard

var Root = "/proc"

func AllowForTest() {}
`
	const helperSrc = `package helper

import "example.com/guard"

func Relax() { guard.AllowForTest() }

func Indirect() { Relax() }

func Move() { guard.Root = "/fake" }
`
	const testSrc = `package user

import "example.com/helper"

func init() { helper.Indirect() } // X1

func init() { helper.Move() } // X2
`
	diags := analyzertest.RunGraphExportView(t, testrelax.New(testrelax.Config{
		Guards: map[string]string{"example.com/guard.AllowForTest": "lets a test skip the check"},
		Vars:   map[string]string{"example.com/guard.Root": "is the guarded root"},
	}), []analyzertest.Package{
		{Path: "example.com/guard", Files: map[string]string{"guard.go": guardSrc}},
		{Path: "example.com/helper", Files: map[string]string{"helper.go": helperSrc}},
		{Path: "example.com/user", Files: map[string]string{"user_test.go": testSrc}},
	})
	var got []string
	for _, d := range diags {
		got = append(got, fmt.Sprintf("%s:%d %s", d.File, d.Line, d.Message))
	}
	files := map[string]string{"helper.go": helperSrc, "user_test.go": testSrc}
	want(t, got, files, map[string]string{
		"X1": "init relaxes example.com/guard.AllowForTest for every test in the package (via Indirect)",
		"X2": "init relaxes example.com/guard.Root for every test in the package (via Move)",
		"":   "Move writes example.com/guard.Root", // helper.go, reported in its own package
	})
}

func contains(got []string, prefix, msg string) bool {
	for _, g := range got {
		if strings.HasPrefix(g, prefix) && strings.Contains(g, msg) {
			return true
		}
	}
	return false
}

// dropPrefixed drops the findings in got with prefix.
func dropPrefixed(got []string, prefix string) []string {
	var out []string
	for _, g := range got {
		if !strings.HasPrefix(g, prefix) {
			out = append(out, g)
		}
	}
	return out
}

// markerLine is the file and line carrying "// marker" among files.
func markerLine(files map[string]string, marker string) (string, int) {
	for name, src := range files {
		for i, line := range strings.Split(src, "\n") {
			if strings.HasSuffix(line, "// "+marker) {
				return name, i + 1
			}
		}
	}
	return "?", -1
}
