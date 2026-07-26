package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The regression under test (ga-rp4k): a bead that lives on a RIG ledger but is
// assigned to a city-scoped agent (gastown.mayor) is invisible to bare
// `bd list --assignee`, because bare bd reads one ledger. `gc beads list`
// already sweeps every rig store plus the city store, so pairing that sweep
// with --assignee is the cross-ledger read. Three P1/P2 delays on 2026-07-26
// traced to the mayor reporting an empty hook while holding assigned
// rig-ledger work.

// newAssigneeTestStore returns an in-memory store seeded with the given beads.
// Assignee and Status are preserved as supplied so the filters under test see
// realistic rows.
func newAssigneeTestStore(t *testing.T, seed []beads.Bead) beads.Store {
	t.Helper()
	store := beads.NewMemStore()
	for _, b := range seed {
		created, err := store.Create(b)
		if err != nil {
			t.Fatalf("seeding bead %q: %v", b.Title, err)
		}
		// Create fills ID/Status/CreatedAt; force the status the case wants.
		if b.Status != "" && created.Status != b.Status {
			if err := store.Update(created.ID, beads.UpdateOpts{Status: &b.Status}); err != nil {
				t.Fatalf("setting status on %q: %v", created.ID, err)
			}
		}
	}
	return store
}

// TestCollectBeadsAcrossStores_AssigneeSpansRigLedgers is the core regression
// pin: an assignee filter must reach beads held in a NON-city store. The city
// store here stands in for the town ledger and the second store for a rig
// ledger; only the rig store holds the mayor's bead, which is exactly the shape
// that made ga-6s9 / ga-0562 invisible.
func TestCollectBeadsAcrossStores_AssigneeSpansRigLedgers(t *testing.T) {
	cityStore := newAssigneeTestStore(t, []beads.Bead{
		{Title: "town bead for someone else", Assignee: "gastown.witness", Status: "open"},
	})
	rigStore := newAssigneeTestStore(t, []beads.Bead{
		{Title: "rig bead for the mayor", Assignee: "gastown.mayor", Status: "open"},
		{Title: "rig bead for a polecat", Assignee: "gascity/gastown.polecat", Status: "open"},
	})
	stores := []convoyStoreView{
		{path: "city", store: cityStore},
		{path: "rig", store: rigStore},
	}

	got, err := collectBeadsAcrossStores(stores, beadFilters{assignee: "gastown.mayor"})
	if err != nil {
		t.Fatalf("collectBeadsAcrossStores: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("assignee sweep returned %d beads, want 1; got=%+v", len(got), got)
	}
	if got[0].Title != "rig bead for the mayor" {
		t.Errorf("assignee sweep returned %q, want the rig-ledger bead", got[0].Title)
	}
	if got[0].Assignee != "gastown.mayor" {
		t.Errorf("returned bead assignee = %q, want %q", got[0].Assignee, "gastown.mayor")
	}
}

// TestCollectBeadsAcrossStores_AssigneeCombinesWithStatus verifies the assignee
// filter composes with --status instead of overriding it. The mayor's startup
// check is an in_progress query, so an assignee sweep that ignored status would
// hand back already-open backlog as if it were resumable work.
func TestCollectBeadsAcrossStores_AssigneeCombinesWithStatus(t *testing.T) {
	rigStore := newAssigneeTestStore(t, []beads.Bead{
		{Title: "mayor in progress", Assignee: "gastown.mayor", Status: "in_progress"},
		{Title: "mayor still open", Assignee: "gastown.mayor", Status: "open"},
		{Title: "other in progress", Assignee: "gastown.witness", Status: "in_progress"},
	})
	stores := []convoyStoreView{{path: "rig", store: rigStore}}

	got, err := collectBeadsAcrossStores(stores, beadFilters{
		assignee: "gastown.mayor",
		status:   "in_progress",
	})
	if err != nil {
		t.Fatalf("collectBeadsAcrossStores: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("assignee+status returned %d beads, want 1; got=%+v", len(got), got)
	}
	if got[0].Title != "mayor in progress" {
		t.Errorf("assignee+status returned %q, want %q", got[0].Title, "mayor in progress")
	}
}

// TestFilterBeads_Assignee covers the client-side filter the API lane relies on.
// The first subtest is the important one: assignee as the ONLY filter must not
// hit filterBeads' empty-filter fast path, which would return every bead in
// town and read as "this agent owns everything".
func TestFilterBeads_Assignee(t *testing.T) {
	all := []beads.Bead{
		{ID: "ga-1", Assignee: "gastown.mayor", Status: "open"},
		{ID: "ga-2", Assignee: "gastown.witness", Status: "open"},
		{ID: "ga-3", Assignee: "gastown.mayor", Status: "in_progress"},
		{ID: "ga-4", Assignee: "", Status: "open"},
	}

	tests := []struct {
		name    string
		filters beadFilters
		wantIDs []string
	}{
		{
			name:    "assignee only is not short-circuited",
			filters: beadFilters{assignee: "gastown.mayor"},
			wantIDs: []string{"ga-1", "ga-3"},
		},
		{
			name:    "assignee with status",
			filters: beadFilters{assignee: "gastown.mayor", status: "in_progress"},
			wantIDs: []string{"ga-3"},
		},
		{
			name:    "assignee matches exactly, not by prefix",
			filters: beadFilters{assignee: "gastown.may"},
			wantIDs: nil,
		},
		{
			name:    "unassigned beads are excluded",
			filters: beadFilters{assignee: "gastown.witness"},
			wantIDs: []string{"ga-2"},
		},
		{
			name:    "no filters still returns everything",
			filters: beadFilters{},
			wantIDs: []string{"ga-1", "ga-2", "ga-3", "ga-4"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := filterBeads(all, tc.filters)
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("filterBeads returned %d beads, want %d; got=%+v", len(got), len(tc.wantIDs), got)
			}
			for i, want := range tc.wantIDs {
				if got[i].ID != want {
					t.Errorf("bead[%d] = %q, want %q", i, got[i].ID, want)
				}
			}
		})
	}
}

// TestParseBeadFilters_Assignee pins both flag spellings and, critically, that
// --assignee is consumed rather than left in the positional remainder where the
// fake-bd harness would treat it as a bead ID.
func TestParseBeadFilters_Assignee(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantAssignee string
		wantRest     []string
	}{
		{
			name:         "equals form",
			args:         []string{"--assignee=gastown.mayor"},
			wantAssignee: "gastown.mayor",
			wantRest:     nil,
		},
		{
			name:         "space-separated form",
			args:         []string{"--assignee", "gastown.mayor"},
			wantAssignee: "gastown.mayor",
			wantRest:     nil,
		},
		{
			name:         "alongside other filters, positional preserved",
			args:         []string{"--status", "in_progress", "--assignee", "gastown.mayor", "ga-xyz"},
			wantAssignee: "gastown.mayor",
			wantRest:     []string{"ga-xyz"},
		},
		{
			name:         "empty assignee value stays empty",
			args:         []string{"--assignee="},
			wantAssignee: "",
			wantRest:     nil,
		},
		{
			name:         "trailing flag with no value is not consumed as a filter",
			args:         []string{"--assignee"},
			wantAssignee: "",
			wantRest:     []string{"--assignee"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, rest := parseBeadFilters(tc.args)
			if got.assignee != tc.wantAssignee {
				t.Errorf("assignee = %q, want %q", got.assignee, tc.wantAssignee)
			}
			if len(rest) != len(tc.wantRest) {
				t.Fatalf("rest = %v, want %v", rest, tc.wantRest)
			}
			for i, want := range tc.wantRest {
				if rest[i] != want {
					t.Errorf("rest[%d] = %q, want %q", i, rest[i], want)
				}
			}
		})
	}
}

// The API-lane assignee filter is the same filterBeads path covered above
// (assignee-only must not short-circuit to "match everything"). An end-to-end
// routeBeadsList + httptest case would grow the untagged http_test_server census
// baseline; that ratchet update is deliberately out of scope for this
// filing-prep PR (ga-fpdi.20 drops the fork census commit).
