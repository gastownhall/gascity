package sling

// Regression test for gastownhall/gascity#8510: when linking the bead to its
// freshly minted auto-convoy fails, sling used to leave that convoy open with
// zero members. Nothing closed it afterwards: gc convoy check skips a convoy
// with no members (TestConvoyCheckEmpty), and a re-sling's reuse/reap set
// only sees convoys that track the bead. The fix (sling_core.go, the
// TrackItem error branch) best-effort closes the convoy it just minted when
// linking fails, mirroring graphv2.CreateSingleItemInputConvoy (#4788).
//
// This reuses failingDepAddStore from sling_test.go, in the same shape as
// TestFinalizeAutoConvoyTracksDepAddError.

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestAutoConvoyClosedWhenTrackFails(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	base := deps.Store
	failing := &failingDepAddStore{
		Store:    base,
		failType: "tracks",
		err:      errors.New("injected DepAdd failure"),
	}
	deps.Store = failing

	b, err := base.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatalf("create bead: %v", err)
	}

	// Two slings while the tracks edge cannot be written.
	for i := 1; i <= 2; i++ {
		result, err := DoSling(SlingOpts{Target: a, BeadOrFormula: b.ID}, deps, deps.Store)
		if err != nil {
			t.Fatalf("sling %d: DoSling: %v", i, err)
		}
		t.Logf("sling %d (tracks fails): err=<nil> ConvoyID=%q MetadataErrors=%q", i, result.ConvoyID, result.MetadataErrors)
	}

	// The store recovers; a third sling links normally.
	deps.Store = base
	result, err := DoSling(SlingOpts{Target: a, BeadOrFormula: b.ID}, deps, deps.Store)
	if err != nil {
		t.Fatalf("sling 3: DoSling: %v", err)
	}
	t.Logf("sling 3 (tracks works): err=<nil> ConvoyID=%q MetadataErrors=%q", result.ConvoyID, result.MetadataErrors)

	convoys, err := base.List(beads.ListQuery{Type: "convoy", IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("list convoys: %v", err)
	}
	var leaked []string
	for _, c := range convoys {
		if c.Title != AutoConvoyRootTitle(b.ID) {
			continue
		}
		members, err := convoycore.Members(base, c.ID, true)
		if err != nil {
			t.Fatalf("members of %s: %v", c.ID, err)
		}
		t.Logf("convoy %s title=%q status=%s members=%d", c.ID, c.Title, c.Status, len(members))
		if c.Status == "open" && len(members) == 0 {
			leaked = append(leaked, c.ID)
		}
	}

	if len(leaked) > 0 {
		t.Fatalf("open auto-convoy(s) %q with zero members left behind by failed links: %v (want the fix to close each one on TrackItem failure)", AutoConvoyRootTitle(b.ID), leaked)
	}
}
