package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// AC-7: the hold runs only with the key set and a crash or unknown previous
// exit.
func TestShouldHoldCitiesAfterUncleanExit(t *testing.T) {
	on := supervisor.Section{HoldCitiesAfterUncleanExit: true}
	for _, tc := range []struct {
		sec  supervisor.Section
		exit string
		want bool
	}{
		{on, supervisor.PreviousExitCrash, true},
		{on, supervisor.PreviousExitUnknown, true},
		{on, supervisor.PreviousExitClean, false},
		{supervisor.Section{}, supervisor.PreviousExitCrash, false},
		{supervisor.Section{}, supervisor.PreviousExitUnknown, false},
	} {
		if got := shouldHoldCitiesAfterUncleanExit(tc.sec, tc.exit); got != tc.want {
			t.Fatalf("key %t previous_exit=%s: hold %t, want %t", tc.sec.HoldCitiesAfterUncleanExit, tc.exit, got, tc.want)
		}
	}
}

// AC-7: each registered city whose directory exists is recorded suspended,
// over an earlier explicit resume, with a city.suspended event naming the
// cause and one stderr line per city naming gc resume.
func TestHoldRegisteredCitiesAfterUncleanExit(t *testing.T) {
	alpha, beta := t.TempDir(), t.TempDir()
	no := false
	if err := suspensionstate.SetCitySuspended(fsys.OSFS{}, beta, &no); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(t.TempDir(), "gone")
	cities := []supervisor.CityEntry{{Path: alpha, Name: "alpha"}, {Path: beta, Name: "beta"}, {Path: gone, Name: "gone"}}
	var stderr strings.Builder
	holdRegisteredCitiesAfterUncleanExit(cities, supervisor.PreviousExitCrash, &stderr)
	for _, c := range cities[:2] {
		st, err := suspensionstate.Load(fsys.OSFS{}, c.Path)
		if err != nil || !suspensionstate.IsCitySuspended(st) {
			t.Fatalf("%s: suspension state %+v (%v), want the city suspended", c.Name, st, err)
		}
		log, err := os.ReadFile(filepath.Join(c.Path, citylayout.RuntimeRoot, "events.jsonl"))
		if err != nil || !strings.Contains(string(log), `"city.suspended"`) || !strings.Contains(string(log), "held: previous_exit=crash") {
			t.Fatalf("%s: events log %q (%v), want city.suspended naming previous_exit=crash", c.Name, log, err)
		}
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Fatalf("a missing city directory was created (%v)", err)
	}
	lines := strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("stderr has %d line(s), want one per held city:\n%s", len(lines), stderr.String())
	}
	for i, c := range cities[:2] {
		if !strings.Contains(lines[i], "gc resume "+c.Path) {
			t.Fatalf("stderr line %q does not name gc resume %s", lines[i], c.Path)
		}
	}
}

// AC-7: a city whose suspension file cannot be written gets the error
// printed and no hold; the next city is still held.
func TestHoldRegisteredCitiesAfterUncleanExitContinuesPastAWriteFailure(t *testing.T) {
	broken, ok := t.TempDir(), t.TempDir()
	// The runtime directory is a file, so the suspension file cannot be written.
	if err := os.WriteFile(filepath.Join(broken, citylayout.RuntimeRoot), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	holdRegisteredCitiesAfterUncleanExit([]supervisor.CityEntry{{Path: broken, Name: "broken"}, {Path: ok, Name: "ok"}}, supervisor.PreviousExitUnknown, &stderr)
	if !strings.Contains(stderr.String(), broken) || !strings.Contains(stderr.String(), "not held") {
		t.Fatalf("stderr %q does not report the failed hold of %s", stderr.String(), broken)
	}
	st, err := suspensionstate.Load(fsys.OSFS{}, ok)
	if err != nil || !suspensionstate.IsCitySuspended(st) {
		t.Fatalf("ok: suspension state %+v (%v), want the city held", st, err)
	}
}
