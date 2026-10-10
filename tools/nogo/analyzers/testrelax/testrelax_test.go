package testrelax_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/internal/analyzertest"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/testrelax"
)

const pkg = "example.com/p"

// src is production code: the guard setter, a seam setter that is no guard,
// and helpers that reach the guard directly, through a chain, or as a value.
const src = `package p

func AllowForTest() (restore func()) { return func() {} }

func SetClockForTest() (restore func()) { return func() {} }

type T struct{}

func (T) Relax() {}

func helper()      { _ = AllowForTest() }
func chain()       { helper() }
func value() func() (restore func()) { return AllowForTest }

func init() { _ = SetClockForTest() }
`

// testSrc is the package's test file.
const testSrc = `package p

type M struct{}

func (M) Run() int { return 0 }

func init() { _ = AllowForTest() } // L1

func init() { chain() } // L2

func TestMain(m M) { _ = value(); _ = m.Run() } // L3

func TestMainLike(m M) { _ = AllowForTest(); _ = m.Run() }

var restore = AllowForTest() // L4

var relax = T{}.Relax // L5

func TestScoped() {
	restore := AllowForTest()
	defer restore()
}
`

func run(t *testing.T, guards map[string]string) []string {
	t.Helper()
	var got []string
	for _, d := range analyzertest.Run(t, testrelax.New(testrelax.Config{Guards: guards}), pkg, map[string]string{"p.go": src, "p_test.go": testSrc}) {
		got = append(got, fmt.Sprintf("%s:%d %s", d.File, d.Line, d.Message))
	}
	return got
}

func TestTestRelax(t *testing.T) {
	got := run(t, map[string]string{
		pkg + ".AllowForTest":   "lets a test skip the check",
		"(" + pkg + ".T).Relax": "relaxes the method's check",
	})
	want := map[string]string{
		"L1": "init relaxes example.com/p.AllowForTest for every test in the package: it lets a test skip the check",
		"L2": "init relaxes example.com/p.AllowForTest for every test in the package (via chain)",
		"L3": "TestMain relaxes example.com/p.AllowForTest for every test in the package (via value)",
		"L4": "a package-level initializer relaxes example.com/p.AllowForTest",
		"L5": "a package-level initializer relaxes (example.com/p.T).Relax",
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
// may be called from init.
func TestTestRelaxIgnoresUnlistedSetters(t *testing.T) {
	if got := run(t, nil); len(got) != 0 {
		t.Fatalf("with no guards listed, findings = %v", got)
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
