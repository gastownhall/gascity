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
// setter that is no guard, and helpers that reach the guard directly, through
// a chain, or as a value.
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

func init() { _ = SetClockForTest(); root = "/proc" }
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

func TestScoped() {
	restore := AllowForTest()
	defer restore()
}

func TestWritesTheVariable() { root = "/fake" } // L7
`

func run(t *testing.T, cfg testrelax.Config) []string {
	t.Helper()
	var got []string
	for _, d := range analyzertest.Run(t, testrelax.New(cfg), pkg, map[string]string{"p.go": src, "p_test.go": testSrc}) {
		got = append(got, fmt.Sprintf("%s:%d %s", d.File, d.Line, d.Message))
	}
	return got
}

func TestTestRelax(t *testing.T) {
	got := run(t, testrelax.Config{
		Guards: map[string]string{
			pkg + ".AllowForTest":   "lets a test skip the check",
			"(" + pkg + ".T).Relax": "relaxes the method's check",
		},
		Vars: map[string]string{pkg + ".root": "is the guarded root"},
	})
	want := map[string]string{
		"L1": "init relaxes example.com/p.AllowForTest for every test in the package: it lets a test skip the check",
		"L2": "init relaxes example.com/p.AllowForTest for every test in the package (via chain)",
		"L3": "TestMain relaxes example.com/p.AllowForTest for every test in the package (via value)",
		"L4": "a package-level initializer relaxes example.com/p.AllowForTest",
		"L5": "a package-level initializer relaxes (example.com/p.T).Relax",
		"L6": "init relaxes example.com/p.root for every test in the package: it is the guarded root",
		"L7": "a test writes example.com/p.root, which is the guarded root; write it through its setter",
	}
	for marker, msg := range want {
		if line := markerLine(marker); !contains(got, fmt.Sprintf("p_test.go:%d ", line), msg) {
			t.Errorf("no finding %q on p_test.go line %d", msg, line)
		}
	}
	if len(got) != len(want) {
		t.Errorf("findings = %d, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
}

// TestTestRelaxIgnoresUnlistedSetters: a seam setter off the reviewed list
// may be called from init, and production may write a guarded variable.
func TestTestRelaxIgnoresUnlistedSetters(t *testing.T) {
	if got := run(t, testrelax.Config{}); len(got) != 0 {
		t.Fatalf("with no guards listed, findings = %v", got)
	}
}

// TestTestRelaxAllowlist: an allowlisted package-global relaxation is not
// reported, and an entry the package no longer needs is.
func TestTestRelaxAllowlist(t *testing.T) {
	got := run(t, testrelax.Config{
		Vars:    map[string]string{pkg + ".root": "is the guarded root"},
		Allowed: map[string]string{pkg + ":" + pkg + ".root": "reviewed", pkg + ":gone": "stale"},
	})
	if len(got) != 1 || !strings.Contains(got[0], "allowlisted gone is not relaxed in this package any more") {
		t.Fatalf("findings = %v, want only the stale entry", got)
	}
}

// TestTestRelaxEnv: init setting a listed break-glass variable, through a
// constant key, is reported; the same call with an unlisted key is not.
func TestTestRelaxEnv(t *testing.T) {
	const osSrc = `package os

func Setenv(key, value string) error { return nil }
`
	const testSrc = `package q

import "os"

const breakGlass = "GC_BREAK_GLASS"

func init() { _ = os.Setenv(breakGlass, "1") } // E1

func init() { _ = os.Setenv("GC_OTHER", "1") }
`
	diags := analyzertest.RunGraph(t, testrelax.New(testrelax.Config{Env: map[string]string{"GC_BREAK_GLASS": "lets a caller past the glass"}}), []analyzertest.Package{
		{Path: "os", Files: map[string]string{"os.go": osSrc}},
		{Path: "example.com/q", Files: map[string]string{"q_test.go": testSrc}},
	})
	if len(diags) != 1 || diags[0].Line != 7 || !strings.Contains(diags[0].Message, "init relaxes GC_BREAK_GLASS for every test in the package: it lets a caller past the glass") {
		t.Fatalf("findings = %+v, want init's set of GC_BREAK_GLASS", diags)
	}
}

// TestTestRelaxAcrossPackages: a helper in another package that reaches a
// guard carries it to an init that calls it, as an export-data driver (nogo,
// go vet) shows the helper's package.
func TestTestRelaxAcrossPackages(t *testing.T) {
	const guardSrc = `package guard

func AllowForTest() {}
`
	const helperSrc = `package helper

import "example.com/guard"

func Relax() { guard.AllowForTest() }

func Indirect() { Relax() }
`
	const testSrc = `package user

import "example.com/helper"

func init() { helper.Indirect() } // X1
`
	diags := analyzertest.RunGraphExportView(t, testrelax.New(testrelax.Config{Guards: map[string]string{"example.com/guard.AllowForTest": "lets a test skip the check"}}), []analyzertest.Package{
		{Path: "example.com/guard", Files: map[string]string{"guard.go": guardSrc}},
		{Path: "example.com/helper", Files: map[string]string{"helper.go": helperSrc}},
		{Path: "example.com/user", Files: map[string]string{"user_test.go": testSrc}},
	})
	if len(diags) != 1 || diags[0].File != "user_test.go" || !strings.Contains(diags[0].Message, "init relaxes example.com/guard.AllowForTest for every test in the package (via Indirect)") {
		t.Fatalf("findings = %+v, want the init reaching the guard through helper.Indirect", diags)
	}
}

func contains(got []string, prefix, msg string) bool {
	for _, g := range got {
		if strings.HasPrefix(g, prefix) && strings.Contains(g, msg) {
			return true
		}
	}
	return false
}

// markerLine is the test source line carrying "// marker".
func markerLine(marker string) int {
	for i, line := range strings.Split(testSrc, "\n") {
		if strings.HasSuffix(line, "// "+marker) {
			return i + 1
		}
	}
	return -1
}
