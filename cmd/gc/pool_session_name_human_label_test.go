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

// Issue #6945: work handed to the operator (assignee "human") after a pool
// claim is a deliberate hand-off, not a dead worker. The core orphan-sweep
// order exempts the exact "human" assignee (#3440); the Go dead-assignee sweep
// must agree and leave the hand-off assigned. An unlabeled dead pool holder is
// the control, so a disabled sweep cannot pass.
func TestReleaseOrphanedPoolAssignments_KeepsWorkHandedToHumanAssignee(t *testing.T) {
	sessionStore := beads.NewMemStore()
	workStore := beads.NewMemStore()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}}

	handoff := seedHumanLabelSweepWork(t, workStore, "pool work handed to the operator", "human", "in_progress")
	control := seedHumanLabelSweepWork(t, workStore, "pool work held by a dead holder", "worker-1", "in_progress")

	released := releaseOrphanedPoolAssignments(
		workStore,
		beads.SessionStore{Store: sessionStore},
		cfg,
		"",
		nil,
		[]beads.Bead{handoff, control},
		[]beads.Store{workStore, workStore},
		nil, nil, nil, nil, nil,
	)

	got, err := workStore.Get(handoff.ID)
	if err != nil {
		t.Fatalf("get hand-off: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "human" {
		t.Errorf("hand-off %s = status %q assignee %q, want in_progress/human untouched (released=%v)", handoff.ID, got.Status, got.Assignee, released)
	}
	if len(released) != 1 || released[0].ID != control.ID {
		t.Fatalf("released = %v, want exactly [%s] (the dead pool holder control)", released, control.ID)
	}
}

// Issue #6945, full shape: only the exact operator identity is a person's
// claim. Every other bare non-session name — an off-roster coordinator, a
// case variant, a prefixed or suffixed lookalike — stays an ordinary dead
// claim the sweep reclaims. One sweep holds both shapes, so a sweep that was
// disabled (keeps everything) or left unchanged (releases every bare name)
// cannot pass. No session exists anywhere, and none answers to a person.
func TestReleaseOrphanedPoolAssignments_KeepsOperatorAssignedClaims(t *testing.T) {
	sessionStore := beads.NewMemStore()
	workStore := beads.NewMemStore()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}}

	handoff := seedHumanLabelSweepWork(t, workStore, "pool work handed to the operator", "human", "in_progress")
	openHandoff := seedHumanLabelSweepWork(t, workStore, "pool work handed to the operator, not started", "human", "open")
	paddedHandoff := seedHumanLabelSweepWork(t, workStore, "pool work handed to the operator with a padded assignee", " human ", "in_progress")
	coordinator := seedHumanLabelSweepWork(t, workStore, "pool work held by an off-roster coordinator", "alice", "in_progress")

	kept := []beads.Bead{handoff, openHandoff, paddedHandoff}
	work := append([]beads.Bead{coordinator}, kept...)
	stores := make([]beads.Store, len(work))
	for i := range stores {
		stores[i] = workStore
	}

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

	releasedByID := make(map[string]bool, len(released))
	for _, r := range released {
		releasedByID[r.ID] = true
	}
	if len(released) != 1 {
		t.Fatalf("released = %v, want exactly [coordinator] (the dead bare-name control)", released)
	}
	for _, k := range kept {
		got, err := workStore.Get(k.ID)
		if err != nil {
			t.Fatalf("get %s: %v", k.ID, err)
		}
		if got.Status != k.Status || got.Assignee != k.Assignee {
			t.Errorf("hand-off %s (%q) = status %q assignee %q, want %q/%q untouched", k.ID, k.Title, got.Status, got.Assignee, k.Status, k.Assignee)
		}
		if releasedByID[k.ID] {
			t.Errorf("hand-off %s (%q) must be absent from the released list", k.ID, k.Title)
		}
	}
	got, err := workStore.Get(coordinator.ID)
	if err != nil {
		t.Fatalf("get control: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Errorf("control %s = status %q assignee %q, want open/unassigned", coordinator.ID, got.Status, got.Assignee)
	}
	if !releasedByID[coordinator.ID] {
		t.Errorf("control %s (%q) must be present in the released list", coordinator.ID, coordinator.Title)
	}
}
