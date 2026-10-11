package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Regression coverage for the hold-label wake/drain loop observed on
// maintainer-city (2026-10-03/04): the on-demand named session `olivia` owned two
// OPEN beads labeled hold:external (auto-undeferred by bd, assignee and hold
// label retained). The controller counted them as assigned-work wake demand, so
// it woke olivia; olivia's `gc hook --claim` refused to serve them
// (isHeldHookCandidate) and drain-acked with reason=no_work; the controller
// stopped the session, emitted session.drain_acked_with_assigned_work, and woke
// it again on the next tick — ~470 times in 24h.
//
// The rule these tests pin: an OPEN assigned bead parked on a dispatch hold is
// not work for its assignee (gas-kg6), so it is not wake demand either. The hook
// and the controller answer with the same predicate (beadmeta.HasDispatchHold).
// In_progress held work stays hold-transparent (ga-5736js): it is a claimed
// ownership fact and keeps its owner visible to recovery.

func heldAssignedOpenWorkCity() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "olivia",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			WorkQuery:         "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "olivia",
			Mode:     "on_demand",
		}},
	}
}

// heldLabelVariants is every canonical hold value plus a case/space variant the
// hook's EqualFold filter also strips.
func heldLabelVariants() []string {
	return append(append([]string(nil), beadmeta.DispatchHoldLabels...), " Hold:External ")
}

func TestBuildDesiredState_OnDemandNamedSession_HeldAssignedOpenWorkIsNotWakeDemand(t *testing.T) {
	for _, label := range heldLabelVariants() {
		t.Run(label, func(t *testing.T) {
			store := beads.NewMemStore()
			held, err := store.Create(beads.Bead{
				Title:    "parked on an external actor",
				Type:     "task",
				Status:   "open",
				Assignee: "olivia",
				Labels:   []string{label},
			})
			if err != nil {
				t.Fatal(err)
			}
			cfg := heldAssignedOpenWorkCity()

			ds := buildDesiredState("test-city", t.TempDir(), time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
			if ds.NamedSessionDemand["olivia"] {
				t.Fatalf("NamedSessionDemand[olivia] = true for an assigned open bead labeled %q; the hook never serves held work, so waking for it is an endless wake/drain loop", label)
			}
			for key, ready := range ds.ReadyAssigned {
				if key.ID == held.ID && ready {
					t.Fatalf("ReadyAssigned[%+v] = true for held bead %s; held open work must carry no wake-demand readiness", key, held.ID)
				}
			}
		})
	}
}

// The control: the identical bead without a hold label still wakes olivia, and a
// held bead next to it does not mask it.
func TestBuildDesiredState_OnDemandNamedSession_UnheldAssignedOpenWorkStillWakesBesideHeld(t *testing.T) {
	store := beads.NewMemStore()
	for _, b := range []beads.Bead{
		{Title: "held", Type: "task", Status: "open", Assignee: "olivia", Labels: []string{beadmeta.HoldExternalLabel}},
		{Title: "actionable", Type: "task", Status: "open", Assignee: "olivia"},
	} {
		if _, err := store.Create(b); err != nil {
			t.Fatal(err)
		}
	}
	ds := buildDesiredState("test-city", t.TempDir(), time.Now().UTC(), heldAssignedOpenWorkCity(), runtime.NewFake(), store, io.Discard)
	if !ds.NamedSessionDemand["olivia"] {
		t.Fatal("NamedSessionDemand[olivia] = false; unheld assigned open work must still wake the named session")
	}
}

// ga-5736js scope pin: an in_progress held bead is a claimed ownership fact, so
// it keeps its owner's demand. Only OPEN held work loses wake readiness.
func TestBuildDesiredState_OnDemandNamedSession_HeldInProgressWorkKeepsDemand(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{
		Title:    "claimed then parked",
		Type:     "task",
		Status:   "open",
		Assignee: "olivia",
		Labels:   []string{beadmeta.HoldExternalLabel},
	})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := store.Update(b.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}
	ds := buildDesiredState("test-city", t.TempDir(), time.Now().UTC(), heldAssignedOpenWorkCity(), runtime.NewFake(), store, io.Discard)
	if !ds.NamedSessionDemand["olivia"] {
		t.Fatal("NamedSessionDemand[olivia] = false for held in_progress work; in_progress ownership stays hold-transparent (ga-5736js)")
	}
}

// The reconciler's own keep-awake probe (idle-timeout and reconciler-owned
// drain-cancel gates) must agree with desired state: held open work alone does
// not keep the session awake.
func TestSessionHasAwakeAssignedWork_HeldOpenWorkIsNotAwakeDemand(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title: "held", Type: "task", Status: "open", Assignee: "olivia",
		Labels: []string{beadmeta.HoldExternalLabel},
	}); err != nil {
		t.Fatal(err)
	}
	has, err := sessionHasAwakeAssignedWorkInStoreByIdentifiers(store, []string{"olivia"})
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("sessionHasAwakeAssignedWorkInStoreByIdentifiers = true for held open work only")
	}

	if _, err := store.Create(beads.Bead{Title: "actionable", Type: "task", Status: "open", Assignee: "olivia"}); err != nil {
		t.Fatal(err)
	}
	has, err = sessionHasAwakeAssignedWorkInStoreByIdentifiers(store, []string{"olivia"})
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("sessionHasAwakeAssignedWorkInStoreByIdentifiers = false with an unheld ready bead beside the held one")
	}
}

// Drain-ack mirror: a session that drained past held open work drained
// correctly (its hook could not serve it), so the open arm of the drain-ack
// anomaly classifier must not report it as a stranded, claimable bead.
func TestDrainAckOpenArm_HeldOpenWorkIsProvablyNonClaimable(t *testing.T) {
	for _, label := range heldLabelVariants() {
		t.Run(label, func(t *testing.T) {
			store := beads.NewMemStore()
			if _, err := store.Create(beads.Bead{
				Title: "held", Type: "task", Status: "open", Assignee: "olivia",
				Labels: []string{label},
			}); err != nil {
				t.Fatal(err)
			}
			got, found, err := firstOpenClaimableAssignedWorkBeadForReachableStore(testSeatWork("", nil, store, nil), sessionpkg.Info{ID: "olivia"}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if found {
				t.Fatalf("drain-ack open arm reported held bead %s (labels %q) as claimable; draining past held work is correct pull", got.ID, got.Labels)
			}
		})
	}
}

func TestDrainAckOpenArm_UnheldOpenWorkBesideHeldStillFires(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title: "held", Type: "task", Status: "open", Assignee: "olivia",
		Labels: []string{beadmeta.HoldMayorLabel},
	}); err != nil {
		t.Fatal(err)
	}
	unheld, err := store.Create(beads.Bead{Title: "actionable", Type: "task", Status: "open", Assignee: "olivia"})
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := firstOpenClaimableAssignedWorkBeadForReachableStore(testSeatWork("", nil, store, nil), sessionpkg.Info{ID: "olivia"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !found || got.ID != unheld.ID {
		t.Fatalf("drain-ack open arm = (%q, %v), want the unheld strand %q", got.ID, found, unheld.ID)
	}
}

// The hook's serve filter and the controller's wake/claimability gates must
// answer with the same predicate, or they drift and the loop returns.
func TestHeldHookCandidateAgreesWithDispatchHoldPredicate(t *testing.T) {
	cases := [][]string{
		nil,
		{"mysql-cutover"},
		{"needs-mayor", "mpr-human-hold"},
		{beadmeta.HoldExternalLabel},
		{"x", beadmeta.HoldMayorLabel},
		{" Hold:External "},
	}
	for _, labels := range cases {
		raw := make([]any, len(labels))
		for i, l := range labels {
			raw[i] = l
		}
		item := map[string]any{"labels": raw}
		bead := beads.Bead{Status: "open", Assignee: "olivia", Labels: labels}
		hook := isHeldHookCandidate(item)
		if want := beadmeta.HasDispatchHold(labels); hook != want {
			t.Errorf("labels %q: isHeldHookCandidate = %v, beadmeta.HasDispatchHold = %v", labels, hook, want)
		}
		if wakes := !assignedOpenWorkHeld(bead); wakes == hook {
			t.Errorf("labels %q: hook serves=%v but controller wake gate counts=%v; they must agree", labels, !hook, wakes)
		}
	}
}
