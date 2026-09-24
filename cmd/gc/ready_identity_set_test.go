package main

import (
	"reflect"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
)

// readyIdentitySetFixture stages one ready bead per identity, created in
// REVERSE priority order so the canonical ready order alone would answer with
// the lowest-priority identity's row. A fixture whose natural order already
// matched the wanted order would let the priority test pass without the sort.
func readyIdentitySetFixture(t *testing.T, identities ...string) (beads.Store, map[string]string) {
	t.Helper()
	store := splittest.NewWorkStore(t, "gc")
	owned := make(map[string]string, len(identities))
	for i := len(identities) - 1; i >= 0; i-- {
		id := identities[i]
		b := mustCreateReadyBead(t, store, beads.Bead{Title: "work for " + id, Type: "task"})
		owner := id
		if err := store.Update(b.ID, beads.UpdateOpts{Assignee: &owner}); err != nil {
			t.Fatalf("assign %s to %q: %v", b.ID, id, err)
		}
		owned[id] = b.ID
	}
	return store, owned
}

// TestReadyAssigneeSetHonorsStatedPriority is the defect case for collapsing the
// caller's per-identity loop. That loop asked each identity in turn and stopped
// at the first hit, so which bead a --limit=1 read returned was decided by the
// ORDER of the identities, not by the ready order. A set that only tested
// membership would answer with whatever sorted first and silently hand the
// session another identity's work.
func TestReadyAssigneeSetHonorsStatedPriority(t *testing.T) {
	const sessionID, sessionName, alias = "s-gcg-1", "gastown.mayor", "mayor"
	store, owned := readyIdentitySetFixture(t, sessionID, sessionName, alias)
	legs := []readyLeg{readyTestLeg("city", store)}

	for _, tt := range []struct {
		name  string
		order []string
		want  string
	}{
		{"session id first", []string{sessionID, sessionName, alias}, owned[sessionID]},
		{"name first", []string{sessionName, alias, sessionID}, owned[sessionName]},
		{"alias first", []string{alias, sessionID, sessionName}, owned[alias]},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := readyBeadsForOpts(legs, readyOpts{assignees: tt.order, limit: 1})
			if err != nil {
				t.Fatalf("gc ready: %v", err)
			}
			got := readyWireIDs(rows)
			if len(got) != 1 || got[0] != tt.want {
				t.Fatalf("--assignee %v --limit=1 served %v, want [%s]: the first stated identity with ready work must win, which is what the loop's early exit did",
					tt.order, got, tt.want)
			}
		})
	}
}

// TestReadyAssigneeSetMatchesPerIdentityQueries is the parity guard the whole
// collapse rests on: one call over the set must serve exactly what the separate
// per-identity calls served between them. Cheaper is worthless if it is also
// narrower, and work that quietly stops being served is the failure this lane
// exists to prevent.
func TestReadyAssigneeSetMatchesPerIdentityQueries(t *testing.T) {
	identities := []string{"s-gcg-1", "gastown.mayor", "mayor", "mayor/"}
	store, owned := readyIdentitySetFixture(t, identities...)
	// A bead owned by nobody in the set and one owned by a stranger: parity
	// must hold by excluding these too, not merely by including ours.
	mustCreateReadyBead(t, store, beads.Bead{Title: "unassigned", Type: "task"})
	stranger := mustCreateReadyBead(t, store, beads.Bead{Title: "stranger", Type: "task"})
	other := "some-other-agent"
	if err := store.Update(stranger.ID, beads.UpdateOpts{Assignee: &other}); err != nil {
		t.Fatalf("assign stranger: %v", err)
	}
	legs := []readyLeg{readyTestLeg("city", store)}

	var looped []string
	for _, id := range identities {
		rows, err := readyBeadsForOpts(legs, readyOpts{assignees: []string{id}})
		if err != nil {
			t.Fatalf("per-identity gc ready for %q: %v", id, err)
		}
		looped = append(looped, readyWireIDs(rows)...)
	}
	single, err := readyBeadsForOpts(legs, readyOpts{assignees: identities})
	if err != nil {
		t.Fatalf("set gc ready: %v", err)
	}
	got := readyWireIDs(single)
	slices.Sort(got)
	slices.Sort(looped)
	if !reflect.DeepEqual(got, looped) {
		t.Fatalf("set call served %v, the per-identity calls served %v: the collapse must not change WHICH work is served, only how many calls it costs",
			got, looped)
	}
	if len(got) != len(identities) {
		t.Fatalf("served %d rows for %d identities (%v): every identity's own bead must survive the collapse",
			len(got), len(identities), got)
	}
	for _, id := range identities {
		if !slices.Contains(got, owned[id]) {
			t.Fatalf("identity %q lost its bead %s from the set call: %v", id, owned[id], got)
		}
	}
}

// TestReadyAssigneeSetCarriesDirtyIdentityForms keeps the set honest about what
// is actually recorded in the data. Ownership was written by more than one
// producer over time, so the same session owns rows under a bare name and a
// trailing-slash form as well as the binding-qualified one. An identity set
// derived from the tidy spellings alone reads as complete and silently drops
// whichever rows carry the untidy ones.
func TestReadyAssigneeSetCarriesDirtyIdentityForms(t *testing.T) {
	dirty := []string{"gastown.mayor", "mayor", "mayor/"}
	store, owned := readyIdentitySetFixture(t, dirty...)
	rows, err := readyBeadsForOpts([]readyLeg{readyTestLeg("city", store)}, readyOpts{assignees: dirty})
	if err != nil {
		t.Fatalf("gc ready: %v", err)
	}
	got := readyWireIDs(rows)
	slices.Sort(got)
	want := []string{owned["gastown.mayor"], owned["mayor"], owned["mayor/"]}
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v: all three recorded spellings are this session's own work and one call must return every one of them", got, want)
	}
}

// TestReadyAssigneeRanksIgnoreBlanksAndRepeats guards the two ways a caller
// assembling the set from environment variables degrades it: an unset variable
// arrives as an empty string, and two variables can hold the same value. A
// blank that ranked would match beads with no assignee at all; a repeat whose
// SECOND mention ranked would let it outrank a later, distinct identity.
func TestReadyAssigneeRanksIgnoreBlanksAndRepeats(t *testing.T) {
	got := readyAssigneeRanks([]string{"", "  ", "mayor", "gastown.mayor", "mayor", " alias "})
	want := map[string]int{"mayor": 0, "gastown.mayor": 1, "alias": 2}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("readyAssigneeRanks = %v, want %v", got, want)
	}
}

// TestReadyOneIdentityIsLeftExactlyAsItWas pins the compatibility edge: every
// caller that exists today names one identity, and for them the set must be a
// pure rename. The sort is skipped below two identities precisely so a
// single-identity read cannot be reordered by this change.
func TestReadyOneIdentityIsLeftExactlyAsItWas(t *testing.T) {
	store := splittest.NewWorkStore(t, "gc")
	owner := "gastown.mayor"
	var ids []string
	for _, title := range []string{"first", "second", "third"} {
		b := mustCreateReadyBead(t, store, beads.Bead{Title: title, Type: "task"})
		if err := store.Update(b.ID, beads.UpdateOpts{Assignee: &owner}); err != nil {
			t.Fatalf("assign %s: %v", b.ID, err)
		}
		ids = append(ids, b.ID)
	}
	legs := []readyLeg{readyTestLeg("city", store)}
	before, err := readyBeadsForOpts(legs, readyOpts{assignees: []string{owner}})
	if err != nil {
		t.Fatalf("gc ready: %v", err)
	}
	// The same rows read again with the identity named twice: the dedupe must
	// make that the one-identity case too, not a two-identity sort.
	twice, err := readyBeadsForOpts(legs, readyOpts{assignees: []string{owner, owner}})
	if err != nil {
		t.Fatalf("gc ready (repeated identity): %v", err)
	}
	if !reflect.DeepEqual(readyWireIDs(before), readyWireIDs(twice)) {
		t.Fatalf("naming the same identity twice reordered the result: %v vs %v",
			readyWireIDs(before), readyWireIDs(twice))
	}
	if len(readyWireIDs(before)) != len(ids) {
		t.Fatalf("served %v, want all %d rows of the single identity", readyWireIDs(before), len(ids))
	}
}

// TestReadyBlankIdentitySetServesNobody is the trap the shell callers will walk
// into when their per-identity loop is collapsed into one call.
//
// The loop skipped blanks and, when every identity was unset, asked for nobody
// and got nothing. Collapsed naively the same session passes --assignee="" three
// times; if the surviving set being empty meant "no assignee filter", a session
// with no identity at all would be handed the whole city's ready work. Presence
// of the flag is what arms the filter, so that cannot happen.
func TestReadyBlankIdentitySetServesNobody(t *testing.T) {
	store := splittest.NewWorkStore(t, "gc")
	mustCreateReadyBead(t, store, beads.Bead{Title: "somebody else's", Type: "task"})
	owned := mustCreateReadyBead(t, store, beads.Bead{Title: "owned", Type: "task"})
	owner := "gastown.mayor"
	if err := store.Update(owned.ID, beads.UpdateOpts{Assignee: &owner}); err != nil {
		t.Fatalf("assign: %v", err)
	}
	legs := []readyLeg{readyTestLeg("city", store)}

	blank, err := readyBeadsForOpts(legs, readyOpts{assignees: []string{"", "", ""}})
	if err != nil {
		t.Fatalf("gc ready: %v", err)
	}
	if got := readyWireIDs(blank); len(got) != 0 {
		t.Fatalf("--assignee with only blank values served %v, want nothing: an identity-less session must be served nobody's work, never everybody's", got)
	}
	// The flag being absent is a different question and must still mean "no
	// assignee filter", or every unfiltered reader would start serving nothing.
	all, err := readyBeadsForOpts(legs, readyOpts{})
	if err != nil {
		t.Fatalf("gc ready without --assignee: %v", err)
	}
	if got := readyWireIDs(all); len(got) != 2 {
		t.Fatalf("no --assignee at all served %v, want both rows: absence of the flag is not an empty set", got)
	}
}
