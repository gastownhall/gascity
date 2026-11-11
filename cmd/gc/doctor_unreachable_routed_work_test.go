package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func runUnreachableRoutedWork(t *testing.T, cityBeads, rigBeads []beads.Bead) *doctor.CheckResult {
	t.Helper()
	cityDir, rigDir := t.TempDir(), t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}
	stores := map[string]beads.Store{
		cityDir: beads.NewMemStoreFrom(0, cityBeads, nil),
		rigDir:  beads.NewMemStoreFrom(0, rigBeads, nil),
	}
	return newUnreachableRoutedWorkCheck(cfg, cityDir, func(path string, q beads.ListQuery) ([]beads.Bead, error) {
		store, ok := stores[path]
		if !ok {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store.List(q)
	}).Run(&doctor.CheckContext{})
}

func routedBead(id, typ, status, assignee, route string) beads.Bead {
	b := beads.Bead{ID: id, Title: id, Type: typ, Status: status, Assignee: assignee}
	if route != "" {
		b.Metadata = map[string]string{"gc.routed_to": route}
	}
	return b
}

func TestUnreachableRoutedWorkFlagsReadyExcludedRoutedBead(t *testing.T) {
	for _, typ := range []string{"step", "molecule"} {
		t.Run(typ, func(t *testing.T) {
			result := runUnreachableRoutedWork(t,
				[]beads.Bead{routedBead("CITY-1", typ, "open", "", "bd.dog")},
				[]beads.Bead{routedBead("RIG-1", typ, "open", "", "repo/polecat")})
			if result.Status != doctor.StatusWarning {
				t.Fatalf("status = %v, want warning: %#v", result.Status, result)
			}
			details := strings.Join(result.Details, "\n")
			for _, want := range []string{
				"city bead CITY-1 (type " + typ + `) is routed to "bd.dog"`,
				"rig repo bead RIG-1 (type " + typ + `) is routed to "repo/polecat"`,
				"controller's demand probe cannot see type",
				"will never be woken by it even though gc hook displays it",
			} {
				if !strings.Contains(details, want) {
					t.Fatalf("details missing %q:\n%s", want, details)
				}
			}
		})
	}
}

func TestUnreachableRoutedWorkIgnoresBeadsThatCanStillRun(t *testing.T) {
	tests := []struct {
		name string
		bead beads.Bead
	}{
		{"actionable type", routedBead("B-1", "task", "open", "", "bd.dog")},
		{"excluded type without a route", routedBead("B-3", "step", "open", "", "")},
		{"excluded type held by an agent", routedBead("B-4", "step", "open", "gascity/builder", "bd.dog")},
		{"excluded type already closed", routedBead("B-5", "step", "closed", "", "bd.dog")},
		{"gate parked on its owner", routedBead("B-6", "gate", "open", "", "mayor")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := runUnreachableRoutedWork(t, []beads.Bead{tt.bead}, nil)
			if result.Status != doctor.StatusOK {
				t.Fatalf("status = %v, want ok: %#v", result.Status, result)
			}
		})
	}
}

func TestUnreachableRoutedWorkIsReportOnly(t *testing.T) {
	check := newUnreachableRoutedWorkCheck(nil, t.TempDir(), nil)
	if check.CanFix() {
		t.Fatal("CanFix = true, want false: closing these beads is a disposition decision")
	}
	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix = %v, want nil no-op", err)
	}
}

func TestUnreachableRoutedWorkReportsUnreadableStore(t *testing.T) {
	cityDir := t.TempDir()
	result := newUnreachableRoutedWorkCheck(nil, cityDir, func(string, beads.ListQuery) ([]beads.Bead, error) {
		return nil, fmt.Errorf("dolt down")
	}).Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning || !strings.Contains(strings.Join(result.Details, "\n"), "dolt down") {
		t.Fatalf("want a skipped-scope warning naming the error, got %#v", result)
	}
}
