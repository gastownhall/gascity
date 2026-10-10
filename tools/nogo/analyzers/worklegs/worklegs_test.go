package worklegs_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/internal/analyzertest"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/worklegs"
)

const beadsSrc = `package beads

type Store interface{ List() }

type WorkStore struct{ Store }

type SessionStore struct{ Store }
`

// location is the defining file: it may set every guarded field.
const location = `package p

import "example.com/beads"

type cityWorkLeg struct{ store beads.Store }

func cityWorkLegOf(w beads.WorkStore) cityWorkLeg { return cityWorkLeg{store: w.Store} }

type WorkLegs struct {
	work beads.Store
	n    int
}

func workLegsFromCensus(w cityWorkLeg) WorkLegs { return WorkLegs{work: w.store} }

type workScope interface{ scopeIDs() []string }

type ReleaseScope struct{ ids []string }
type RefuseScope struct{ ids []string }

func (s ReleaseScope) scopeIDs() []string { return s.ids }
func (s RefuseScope) scopeIDs() []string  { return s.ids }
`

const callers = `package p

import "example.com/beads"

type runtime struct{}

func (runtime) legs(w beads.WorkStore) WorkLegs { return workLegsFromCensus(cityWorkLegOf(w)) }

func stranger(w beads.WorkStore) WorkLegs { return workLegsFromCensus(cityWorkLegOf(w)) } // M1 M2

func viaValue(w beads.WorkStore) cityWorkLeg { f := cityWorkLegOf; return f(w) } // M3

type alias = WorkLegs

func zeroSet(s beads.Store) WorkLegs { var l WorkLegs; l.work = s; return l } // F1

func copySet(l WorkLegs) WorkLegs { c := l; c.n++; return c } // F2

func addressSet(l *WorkLegs) *beads.Store { return &l.work } // F3

func elided(s beads.Store) []alias { return []alias{{work: s}} } // F4

func zeroOK() WorkLegs { return WorkLegs{} }

func fromSession(s beads.SessionStore) beads.WorkStore { return beads.WorkStore{Store: s.Store} } // S1

func converted(s beads.SessionStore) beads.WorkStore { return beads.WorkStore(s) } // S2

func fromWork(s beads.Store) beads.WorkStore { return beads.WorkStore{Store: s} }

type wide struct{ RefuseScope } // I1

type loose struct{} // I2

func (loose) scopeIDs() []string { return nil }
`

var cfg = worklegs.Config{
	Package:      "example.com/p",
	DefiningFile: "location.go",
	Guarded:      []string{"WorkLegs", "cityWorkLeg"},
	Mints: map[string]map[string]string{
		"cityWorkLegOf":      {"runtime.legs": "the runtime's work store", "gone": "a stale entry"},
		"workLegsFromCensus": {"runtime.legs": "over its cityWorkLegOf"},
	},
	Scope:        "workScope",
	ScopeImpls:   []string{"ReleaseScope", "RefuseScope"},
	WorkStore:    "example.com/beads.WorkStore",
	SessionStore: "example.com/beads.SessionStore",
}

func TestWorkLegsLint(t *testing.T) {
	var got []string
	for _, d := range analyzertest.RunGraph(t, worklegs.New(cfg), []analyzertest.Package{
		{Path: "example.com/beads", Files: map[string]string{"beads.go": beadsSrc}},
		{Path: "example.com/p", Files: map[string]string{"location.go": location, "callers.go": callers, "fixture_test.go": "package p\n\nfunc fixture() WorkLegs { return WorkLegs{n: 1} }\n"}},
	}) {
		got = append(got, fmt.Sprintf("%s:%d %s", d.File, d.Line, d.Message))
	}
	want := map[string]string{
		"M1": "cityWorkLegOf referenced in stranger",
		"M2": "workLegsFromCensus referenced in stranger",
		"M3": "cityWorkLegOf referenced in viaValue",
		"F1": "WorkLegs.work set outside location.go",
		"F2": "WorkLegs.n set outside location.go",
		"F3": "WorkLegs.work set outside location.go",
		"F4": "WorkLegs literal with fields outside location.go",
		"S1": "a work store built from a sessions store",
		"S2": "a work store converted from a sessions store",
		"I1": "wide implements workScope",
		"I2": "loose implements workScope",
		"":   "reviewed mint site gone (cityWorkLegOf) mints nothing",
	}
	for marker, msg := range want {
		prefix := "callers.go:" + fmt.Sprint(markerLine(marker)) + " "
		if marker == "" {
			prefix = ""
		}
		if !contains(got, prefix, msg) {
			t.Errorf("no finding %q at %q", msg, prefix)
		}
	}
	if len(got) != len(want) {
		t.Errorf("findings = %d, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
}

func TestWorkLegsLintOutOfScope(t *testing.T) {
	c := cfg
	c.Package = "example.com/other"
	if got := analyzertest.RunGraph(t, worklegs.New(c), []analyzertest.Package{
		{Path: "example.com/beads", Files: map[string]string{"beads.go": beadsSrc}},
		{Path: "example.com/p", Files: map[string]string{"location.go": location, "callers.go": callers}},
	}); len(got) != 0 {
		t.Fatalf("an unlinted package reported %v", got)
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

// markerLine is the callers.go line whose trailing comment names marker.
func markerLine(marker string) int {
	for i, line := range strings.Split(callers, "\n") {
		if _, comment, ok := strings.Cut(line, "// "); ok && slices.Contains(strings.Fields(comment), marker) {
			return i + 1
		}
	}
	return -1
}
