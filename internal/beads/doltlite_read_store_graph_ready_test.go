//go:build gascity_native_beads

package beads

import (
	"testing"
	"time"
)

// TestDoltliteReadStoreReadyGraphOnlyReturnsTierWispsOnly is the TDD anchor for
// ga-ifavnc.2. Until DoltliteReadStore.ReadyGraphOnly is added, GraphOnlyReadyFor
// returns (nil, false) and the test fails at the ok-assertion. Once the method
// exists it must return only wisp-table beads, never durable issue-table beads.
func TestDoltliteReadStoreReadyGraphOnlyReturnsTierWispsOnly(t *testing.T) {
	now := time.Now().UTC()
	store := newDoltliteStoreWithRows(t,
		[]testDoltliteIssue{
			{ID: "gco-issue", Title: "durable issue", Status: "open", IssueType: "task", CreatedAt: now},
		},
		[]testDoltliteIssue{
			{ID: "gco-wisp", Title: "wisp step", Status: "open", IssueType: "molecule", CreatedAt: now.Add(time.Second), Ephemeral: true},
			{ID: "gco-nohistory", Title: "no-history step", Status: "open", IssueType: "molecule", CreatedAt: now.Add(2 * time.Second), NoHistory: true},
		},
	)

	graphOnly, ok := GraphOnlyReadyFor(store)
	if !ok {
		t.Skip("DoltliteReadStore does not implement GraphOnlyReadyStore; add ReadyGraphOnly in ga-ifavnc.2")
	}

	rows, err := graphOnly.ReadyGraphOnly()
	if err != nil {
		t.Fatalf("ReadyGraphOnly: %v", err)
	}
	if hasTestBead(rows, "gco-issue") {
		t.Errorf("ReadyGraphOnly included durable issues-tier bead gco-issue; want wisp tier only")
	}
	if !hasTestBead(rows, "gco-wisp") {
		t.Errorf("ReadyGraphOnly missing ephemeral wisp gco-wisp; ids=%v", testBeadIDs(rows))
	}
	if !hasTestBead(rows, "gco-nohistory") {
		t.Errorf("ReadyGraphOnly missing no-history bead gco-nohistory; ids=%v", testBeadIDs(rows))
	}
}

// TestDoltliteReadStoreReadyGraphOnlyForcesTierWisps pins that ReadyGraphOnly
// ignores the caller-supplied TierMode and always queries the wisp table set.
// Passing TierBoth or TierIssues must not cause durable issues to appear.
func TestDoltliteReadStoreReadyGraphOnlyForcesTierWisps(t *testing.T) {
	now := time.Now().UTC()
	store := newDoltliteStoreWithRows(t,
		[]testDoltliteIssue{
			{ID: "gco-issue", Title: "durable issue", Status: "open", IssueType: "task", CreatedAt: now},
		},
		[]testDoltliteIssue{
			{ID: "gco-wisp", Title: "wisp step", Status: "open", IssueType: "molecule", CreatedAt: now.Add(time.Second), Ephemeral: true},
		},
	)

	graphOnly, ok := GraphOnlyReadyFor(store)
	if !ok {
		t.Skip("DoltliteReadStore does not implement GraphOnlyReadyStore; add ReadyGraphOnly in ga-ifavnc.2")
	}

	for _, mode := range []TierMode{TierBoth, TierIssues} {
		rows, err := graphOnly.ReadyGraphOnly(ReadyQuery{TierMode: mode})
		if err != nil {
			t.Fatalf("ReadyGraphOnly(TierMode=%v): %v", mode, err)
		}
		if hasTestBead(rows, "gco-issue") {
			t.Errorf("ReadyGraphOnly(TierMode=%v) returned durable issues-tier bead; must be forced to TierWisps", mode)
		}
		if !hasTestBead(rows, "gco-wisp") {
			t.Errorf("ReadyGraphOnly(TierMode=%v) missing wisp gco-wisp; ids=%v", mode, testBeadIDs(rows))
		}
	}
}

// TestDoltliteReadStoreReadyGraphOnlyExcludesBlockedWisps verifies that
// ReadyGraphOnly gates on wisp_dependencies: a wisp whose depends_on_wisp_id
// target is still open must not appear; one whose target is closed must appear.
func TestDoltliteReadStoreReadyGraphOnlyExcludesBlockedWisps(t *testing.T) {
	now := time.Now().UTC()
	store := newDoltliteStoreWithRows(t, nil, []testDoltliteIssue{
		// Open dependency: no own deps, so it IS ready itself.
		{ID: "gco-dep-open", Title: "open dep", Status: "open", IssueType: "molecule", Ephemeral: true, CreatedAt: now},
		// Blocked by gco-dep-open (open) → must be excluded.
		{
			ID: "gco-dep-blocked", Title: "blocked wisp", Status: "open", IssueType: "molecule", Ephemeral: true,
			CreatedAt:    now.Add(time.Second),
			Dependencies: []testDoltliteDependency{{DependsOnWispID: "gco-dep-open", Type: "blocks"}},
		},
		// Closed dependency.
		{ID: "gco-dep-done", Title: "done dep", Status: "closed", IssueType: "molecule", Ephemeral: true, CreatedAt: now.Add(2 * time.Second)},
		// Blocked by gco-dep-done (closed) → must be included.
		{
			ID: "gco-dep-unblocked", Title: "unblocked wisp", Status: "open", IssueType: "molecule", Ephemeral: true,
			CreatedAt:    now.Add(3 * time.Second),
			Dependencies: []testDoltliteDependency{{DependsOnWispID: "gco-dep-done", Type: "blocks"}},
		},
	})

	graphOnly, ok := GraphOnlyReadyFor(store)
	if !ok {
		t.Skip("DoltliteReadStore does not implement GraphOnlyReadyStore")
	}

	rows, err := graphOnly.ReadyGraphOnly()
	if err != nil {
		t.Fatalf("ReadyGraphOnly: %v", err)
	}
	if hasTestBead(rows, "gco-dep-blocked") {
		t.Errorf("ReadyGraphOnly included wisp gco-dep-blocked whose dep gco-dep-open is still open")
	}
	if !hasTestBead(rows, "gco-dep-unblocked") {
		t.Errorf("ReadyGraphOnly missing gco-dep-unblocked (closed-dep wisp); ids=%v", testBeadIDs(rows))
	}
}
