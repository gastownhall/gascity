package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// seedHumanLabelSweepWork creates one pool-routed work bead in store with the
// given assignee, status and labels.
func seedHumanLabelSweepWork(t *testing.T, store beads.Store, title, assignee, status string, labels ...string) beads.Bead {
	t.Helper()
	work, err := store.Create(beads.Bead{
		Title:    title,
		Type:     "task",
		Assignee: assignee,
		Labels:   labels,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
	})
	if err != nil {
		t.Fatalf("create %q: %v", title, err)
	}
	if status != "open" {
		if err := store.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("set %q status %q: %v", title, status, err)
		}
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("reload %q: %v", title, err)
	}
	return work
}

// A bead carrying bd's native human label waits on a person. Route-scoped
// dispatch already refuses to serve it (PoolDemandServeRulesForQuery), so the
// dead-assignee sweep has no pool lane to give it back to: releasing it only
// wipes the claim. A person is never a session, so "no session answers to this
// assignee" is no evidence that the claim is dead.
//
// One sweep holds every shape and an unlabeled control, so a sweep that was
// disabled (keeps everything) or left unchanged (releases every bare name)
// cannot pass. Sessions live in their own class store, as on a split city.
func TestReleaseOrphanedPoolAssignments_KeepsHumanLabeledClaims(t *testing.T) {
	sessionStore := beads.NewMemStore()
	workStore := beads.NewMemStore()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}}

	personClaim := seedHumanLabelSweepWork(t, workStore, "human step claimed by a person", "alice", "in_progress", beadmeta.HumanLabel)
	qualifiedClaim := seedHumanLabelSweepWork(t, workStore, "human step claimed under a qualified identity", "human/alice", "in_progress", beadmeta.HumanLabel)
	openClaim := seedHumanLabelSweepWork(t, workStore, "human step assigned but not started", "alice", "open", beadmeta.HumanLabel)
	startedUnassigned := seedHumanLabelSweepWork(t, workStore, "human step started with no assignee", "", "in_progress", beadmeta.HumanLabel)
	control := seedHumanLabelSweepWork(t, workStore, "pool work held by a dead holder", "alice", "in_progress")

	work := []beads.Bead{personClaim, qualifiedClaim, openClaim, startedUnassigned, control}
	stores := []beads.Store{workStore, workStore, workStore, workStore, workStore}

	logBuf := captureSweepLog(t)
	released := releaseOrphanedPoolAssignments(
		workStore,
		beads.SessionStore{Store: sessionStore},
		cfg,
		"",
		nil,
		work,
		stores,
		nil, nil, nil, nil, nil,
	)

	if len(released) != 1 || released[0].ID != control.ID {
		t.Fatalf("released = %v, want exactly [%s] (the unlabeled control)", released, control.ID)
	}
	for _, kept := range []beads.Bead{personClaim, qualifiedClaim, openClaim, startedUnassigned} {
		got, err := workStore.Get(kept.ID)
		if err != nil {
			t.Fatalf("get %s: %v", kept.ID, err)
		}
		if got.Status != kept.Status || got.Assignee != kept.Assignee {
			t.Errorf("%s (%q) = status %q assignee %q, want %q/%q untouched", kept.ID, kept.Title, got.Status, got.Assignee, kept.Status, kept.Assignee)
		}
	}
	got, err := workStore.Get(control.ID)
	if err != nil {
		t.Fatalf("get control: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("control = status %q assignee %q, want open/unassigned", got.Status, got.Assignee)
	}
	// The label is decided before the foreign-claim gate, so a qualified
	// person identity is not reported as a protected foreign claim every tick.
	if logged := logBuf.String(); strings.Contains(logged, "protected") {
		t.Fatalf("a human-labeled claim must not reach the foreign-claim gate; log:\n%s", logged)
	}
}

// runOperatorSweep runs one dead-assignee sweep over work, which lives in
// workStore and is routed to the pool agent "worker", with no session
// anywhere. It returns the released bead IDs.
func runOperatorSweep(t *testing.T, workStore beads.Store, work ...beads.Bead) map[string]bool {
	t.Helper()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}}
	stores := make([]beads.Store, len(work))
	for i := range stores {
		stores[i] = workStore
	}
	released := releaseOrphanedPoolAssignments(
		workStore,
		beads.SessionStore{Store: beads.NewMemStore()},
		cfg,
		"",
		nil,
		work,
		stores,
		nil, nil, nil, nil, nil,
	)
	ids := make(map[string]bool, len(released))
	for _, r := range released {
		ids[r.ID] = true
	}
	return ids
}

// assertSweepKept fails unless each bead is absent from released and still
// has the status and assignee it was seeded with.
func assertSweepKept(t *testing.T, store beads.Store, released map[string]bool, kept ...beads.Bead) {
	t.Helper()
	for _, want := range kept {
		got, err := store.Get(want.ID)
		if err != nil {
			t.Fatalf("get %s: %v", want.ID, err)
		}
		if released[want.ID] {
			t.Errorf("%s (assignee %q) is in the released list, want it kept", want.ID, want.Assignee)
		}
		if got.Status != want.Status || got.Assignee != want.Assignee {
			t.Errorf("%s = status %q assignee %q, want %q/%q untouched", want.ID, got.Status, got.Assignee, want.Status, want.Assignee)
		}
	}
}

// assertSweepReleased fails unless each bead is in released and is now open
// with no assignee.
func assertSweepReleased(t *testing.T, store beads.Store, released map[string]bool, gone ...beads.Bead) {
	t.Helper()
	for _, want := range gone {
		got, err := store.Get(want.ID)
		if err != nil {
			t.Fatalf("get %s: %v", want.ID, err)
		}
		if !released[want.ID] {
			t.Errorf("%s (assignee %q) is missing from the released list", want.ID, want.Assignee)
		}
		if got.Status != "open" || got.Assignee != "" {
			t.Errorf("%s (was %q) = status %q assignee %q, want open/unassigned", want.ID, want.Assignee, got.Status, got.Assignee)
		}
	}
}

// Issue #6945: `bd update <id> --assignee human` hands claimed pool work to the
// operator without setting the human label. No session answers to the
// operator, so the sweep must not read the missing session as a dead claim;
// the core orphan-sweep order keeps the same bead (#3440). The sweep trims
// assignees, so a padded operator name is the operator too. A dead pool holder
// is the control, so a disabled sweep cannot pass.
func TestReleaseOrphanedPoolAssignments_KeepsOperatorAssignedClaims(t *testing.T) {
	workStore := beads.NewMemStore()
	handoff := seedHumanLabelSweepWork(t, workStore, "pool work handed to the operator", "human", "in_progress")
	padded := seedHumanLabelSweepWork(t, workStore, "pool work handed to a padded operator name", " human ", "in_progress")
	control := seedHumanLabelSweepWork(t, workStore, "pool work held by a dead holder", "worker-1", "in_progress")

	released := runOperatorSweep(t, workStore, handoff, padded, control)

	assertSweepKept(t, workStore, released, handoff, padded)
	assertSweepReleased(t, workStore, released, control)
}
