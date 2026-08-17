package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestBuildDesiredState_MultiSlotPoolNamedSession_OneRoutedBeadProvisionsTwoWorkers
// is the ga-80pen8 repro: two gascity/builder identities (the [[named_session]]
// and a pool slot) concurrently claimed+completed the same routed bead.
//
// Root cause: an agent template configured as BOTH a [[named_session]] AND a
// multi-slot pool (max_active_sessions > 1, so UsesCanonicalSingletonPoolIdentity()
// is false and the #3697 canonical-singleton standby suppression does not apply)
// overloads ONE identity string ("gascity/builder") as:
//   - the named-session wake identity,
//   - the pool template / gc.routed_to queue key,
//   - the pool claim assignee (bd update --claim --assignee="$GC_TEMPLATE"), and
//   - the Tier-1 crash-recovery key (bd list --assignee="$GC_TEMPLATE" --status=in_progress).
//
// A single routed bead, once a pool slot claims it, carries BOTH
// gc.routed_to=gascity/builder AND Assignee=gascity/builder (the bare template —
// exactly how live ga-frpt4k ended up). On the reconcile tick that one bead then
// drives TWO independent demand paths:
//   - filterAssignedWorkBeadsForPoolDemand (gc.routed_to) -> a pool slot, and
//   - namedWorkReady (build_desired_state.go ~892, `if assignee != identity`)
//     (Assignee==identity) -> the on_demand named session.
//
// so buildDesiredState provisions BOTH gascity/builder (named) and
// gascity/builder-1 (pool slot) for the same unit of work. Both then run the
// shared claim/Tier-1 protocol and work the bead to completion — the observed
// double-claim, duplicate commit, duplicate reviewer handoff, and duplicate
// bd close.
//
// Invariant (fix-agnostic): one unit of routed work must not simultaneously
// provision the named session AND a pool slot of the same template. Holds under
// every candidate fix — pool claims under its concrete alias (so Assignee is
// gascity/builder-1, not the bare template, and namedWorkReady no longer
// matches); the reconciler excludes multi-slot-pool self-claims from named
// demand; or config forbids a [[named_session]] on a max>1 pool template.
//
// MUST fail on current code (2 desired builder sessions) and pass after the fix.
func TestBuildDesiredState_MultiSlotPoolNamedSession_OneRoutedBeadProvisionsTwoWorkers(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "gascity")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	// One routed bead, already claimed by a pool slot: the claim stamped the bare
	// template as assignee (bd update --claim --assignee="$GC_TEMPLATE") and moved
	// it to in_progress — mirroring live ga-frpt4k (Assignee: gascity/builder,
	// gc.routed_to: gascity/builder).
	b, err := rigStore.Create(beads.Bead{
		Title:    "routed builder work claimed by a pool slot",
		Type:     "task",
		Status:   "open",
		Assignee: "gascity/builder",
		Metadata: map[string]string{"gc.routed_to": "gascity/builder"},
	})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := rigStore.Update(b.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "gc"},
		Rigs:      []config.Rig{{Name: "gascity", Path: rigPath}},
		Agents: []config.Agent{{
			Name:              "builder",
			Dir:               "gascity",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(3), // multi-slot pool: not a canonical singleton
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "builder",
			Dir:      "gascity",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredStateWithSessionBeads(
		"gc", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, map[string]beads.Store{"gascity": rigStore}, nil, nil, io.Discard,
	)

	var builderIdentities []string
	for key, tp := range dsResult.State {
		if tp.TemplateName == "gascity/builder" {
			builderIdentities = append(builderIdentities, key+"(alias="+tp.Alias+",named="+tp.ConfiguredNamedIdentity+")")
		}
	}
	if len(builderIdentities) != 1 {
		t.Fatalf("one routed builder bead provisioned %d gascity/builder sessions, want 1: %v\n"+
			"The bead's Assignee==bare template fired named-session demand (namedWorkReady) while its "+
			"gc.routed_to fired pool demand, so the [[named_session]] gascity/builder AND pool slot "+
			"gascity/builder-1 are both provisioned for the same unit of work — the ga-80pen8 "+
			"double-claim. NamedSessionDemand=%v",
			len(builderIdentities), builderIdentities, dsResult.NamedSessionDemand)
	}
}

// TestSharedTemplateAssignee_Tier1CrashRecoveryCrossAdopts proves the direct
// mechanism by which the two provisioned sessions both do the work: the Tier-1
// crash-recovery query every builder session runs,
//
//	bd list --assignee="$GC_TEMPLATE" --status=in_progress
//
// keys on the shared $GC_TEMPLATE ("gascity/builder"). A bead a sibling session
// already claimed (Assignee="gascity/builder", in_progress) is returned by that
// query, so a second live session "resumes" it as if it were its own abandoned
// work. The store query is the exact surface bd wraps.
//
// The companion assertion shows the fix direction: had the pool slot claimed
// under its concrete alias ("gascity/builder-1"), the named session's Tier-1
// scan (assignee="gascity/builder") would NOT surface it — no cross-adoption.
//
// This test does not exercise the reconciler guard (that's the test above) —
// it documents the *other*, prompt-level failure surface (ga-i1d0tr) that the
// guard does not fix on its own. Kept unmodified as a regression guard per
// ga-n2szjj's done-when.
func TestSharedTemplateAssignee_Tier1CrashRecoveryCrossAdopts(t *testing.T) {
	store := beads.NewMemStore()

	// Sibling A (a pool slot) claimed shared routed work under the bare template.
	shared, err := store.Create(beads.Bead{Title: "shared-claim", Type: "task", Status: "open", Assignee: "gascity/builder"})
	if err != nil {
		t.Fatal(err)
	}
	// Sibling B (a hypothetical fixed pool slot) claimed under its concrete alias.
	concrete, err := store.Create(beads.Bead{Title: "concrete-claim", Type: "task", Status: "open", Assignee: "gascity/builder-1"})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := "in_progress"
	for _, id := range []string{shared.ID, concrete.ID} {
		if err := store.Update(id, beads.UpdateOpts{Status: &inProgress}); err != nil {
			t.Fatal(err)
		}
	}

	// The named session's Tier-1 crash-recovery query: assignee == $GC_TEMPLATE.
	tier1, err := store.List(beads.ListQuery{Assignee: "gascity/builder", Status: "in_progress"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, b := range tier1 {
		ids = append(ids, b.ID)
	}

	sawShared, sawConcrete := false, false
	for _, id := range ids {
		if id == shared.ID {
			sawShared = true
		}
		if id == concrete.ID {
			sawConcrete = true
		}
	}
	if !sawShared {
		t.Fatalf("Tier-1 (assignee=gascity/builder) did not return the shared-template claim %s; ids=%v", shared.ID, ids)
	}
	if sawConcrete {
		t.Fatalf("Tier-1 (assignee=gascity/builder) returned the concrete-alias claim %s — it must not; "+
			"claiming under a concrete pool identity is what prevents cross-adoption; ids=%v", concrete.ID, ids)
	}
	// sawShared==true: a sibling's active work is indistinguishable from this
	// session's own crash-recovery work under the shared $GC_TEMPLATE assignee.
	t.Logf("Tier-1 crash-recovery surfaced a live sibling's shared-template claim %s (cross-adoption); "+
		"a concrete-alias claim %s was correctly invisible", shared.ID, concrete.ID)
}

// TestBuildDesiredState_NamedSessionBareIdentityReaped_RecoversViaNamedTier is
// the ga-p0u752 target scenario: a named+expanded-identity session's canonical
// bead is gone (reaped/closed, or never materialized this tick) while it
// still holds an in-progress work bead under its own bare identity. Same
// fixture shape as TestBuildDesiredState_MultiSlotPoolNamedSession_OneRoutedBeadProvisionsTwoWorkers
// (which only asserts the *count* stays 1), but this test asserts *which*
// tier recovers it: after the fix, the named-session tier must recover the
// bead (a session materializes with ConfiguredNamedIdentity set) and pool
// must NOT also spin up a competing wake-known-identity worker for the same
// bead — the actual double-provisioning ga-i1d0tr/ga-80pen8 was about.
//
// Supersedes TestNamedWorkReady_ExpandedIdentityTemplate_NoCanonicalBead_DoesNotMaterialize
// (added on main by e1ba0a131), which pinned the opposite outcome for this
// same fixture shape under ga-tpe9od's original "Candidate A not yet live"
// premise. That test queued for days behind an unrelated prerequisite PR and
// landed on main on 2026-07-06 — two days after ga-wfcfoe (2026-07-04)
// confirmed Candidate A live fleet-wide and authorized removing the guard via
// this fix (ga-p0u752, closed 2026-07-04) — so it merged already asserting a
// stale premise. See bd show ga-wfcfoe and bd show ga-p0u752.
func TestBuildDesiredState_NamedSessionBareIdentityReaped_RecoversViaNamedTier(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "gascity")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	b, err := rigStore.Create(beads.Bead{
		Title:    "routed builder work, named session's canonical bead reaped",
		Type:     "task",
		Status:   "open",
		Assignee: "gascity/builder",
		Metadata: map[string]string{"gc.routed_to": "gascity/builder"},
	})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := rigStore.Update(b.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "gc"},
		Rigs:      []config.Rig{{Name: "gascity", Path: rigPath}},
		Agents: []config.Agent{{
			Name:              "builder",
			Dir:               "gascity",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(3), // multi-slot pool: not a canonical singleton
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "builder",
			Dir:      "gascity",
			Mode:     "on_demand",
		}},
	}

	// No session beads at all: the named session's own canonical bead is gone.
	dsResult := buildDesiredStateWithSessionBeads(
		"gc", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, map[string]beads.Store{"gascity": rigStore}, nil, nil, io.Discard,
	)

	var builderIdentities []string
	namedCount := 0
	for key, tp := range dsResult.State {
		if tp.TemplateName == "gascity/builder" {
			builderIdentities = append(builderIdentities, key+"(alias="+tp.Alias+",named="+tp.ConfiguredNamedIdentity+")")
			if tp.ConfiguredNamedIdentity != "" {
				namedCount++
			}
		}
	}
	if len(builderIdentities) != 1 {
		t.Fatalf("got %d gascity/builder sessions, want exactly 1: %v", len(builderIdentities), builderIdentities)
	}
	if namedCount != 1 {
		t.Fatalf("the one gascity/builder session was not recovered via the named-session tier "+
			"(ConfiguredNamedIdentity unset) — pool spun up a competing worker instead: %v", builderIdentities)
	}
}
