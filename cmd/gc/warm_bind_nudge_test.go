package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

const warmClaimText = "Run gc hook --claim --json now; if it returns work, execute the claimed formula immediately."

func warmBindPoolSession() *beads.Bead {
	return &beads.Bead{
		ID:     "s-1",
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"session_name":                    "worker-1",
			"pool_managed":                    "true",
			beadmeta.TriggerBeadIDMetadataKey: "w-1",
		},
	}
}

func alwaysUnclaimed(beads.Bead) bool { return true }
func neverUnclaimed(beads.Bead) bool  { return false }

// countNudges returns how many Nudge calls the fake recorded and the message of
// the last one.
func countNudges(sp *runtime.Fake) (int, string) {
	n, last := 0, ""
	for _, c := range sp.Calls {
		if c.Method == "Nudge" {
			n++
			last = c.Message
		}
	}
	return n, last
}

// A warm slot with a newly-bound, unclaimed trigger is nudged exactly once with
// the claim text and the marker is persisted; a second pass does not re-nudge
// (marker guard); binding a different trigger fires exactly one more nudge.
func TestDeliverWarmBindClaimNudge_FiresOncePerBinding(t *testing.T) {
	sp := runtime.NewFake()
	// tmux-like default: activity reporting ON. The hook must still fire — proving
	// it is provider-agnostic, not gated on CanReportActivity.
	if !sp.Capabilities().CanReportActivity {
		t.Fatal("precondition: default fake should report activity (tmux-like)")
	}
	session := warmBindPoolSession()
	store := beads.NewMemStoreFrom(0, []beads.Bead{*session}, nil)

	// Pass 1: fires once, stamps the marker.
	deliverWarmBindClaimNudge(context.Background(), sp, store, session, warmClaimText, alwaysUnclaimed)
	if n, last := countNudges(sp); n != 1 || last != warmClaimText {
		t.Fatalf("pass 1: got %d nudges (last=%q), want 1 with claim text", n, last)
	}
	if got := session.Metadata[warmBindNudgedForTriggerKey]; got != "w-1" {
		t.Fatalf("pass 1: in-memory marker = %q, want w-1", got)
	}
	stored, _ := store.Get("s-1")
	if got := stored.Metadata[warmBindNudgedForTriggerKey]; got != "w-1" {
		t.Fatalf("pass 1: persisted marker = %q, want w-1", got)
	}

	// Pass 2: same binding — marker matches, no re-nudge.
	deliverWarmBindClaimNudge(context.Background(), sp, store, session, warmClaimText, alwaysUnclaimed)
	if n, _ := countNudges(sp); n != 1 {
		t.Fatalf("pass 2: got %d nudges, want still 1 (marker guard)", n)
	}

	// Rebind to a different trigger — fires exactly once more.
	session.Metadata[beadmeta.TriggerBeadIDMetadataKey] = "w-2"
	deliverWarmBindClaimNudge(context.Background(), sp, store, session, warmClaimText, alwaysUnclaimed)
	if n, last := countNudges(sp); n != 2 || last != warmClaimText {
		t.Fatalf("rebind: got %d nudges (last=%q), want 2 with claim text", n, last)
	}
	if got := session.Metadata[warmBindNudgedForTriggerKey]; got != "w-2" {
		t.Fatalf("rebind: marker = %q, want w-2", got)
	}
}

// The idle-ready gate runs before delivery so the nudge never lands mid-turn.
func TestDeliverWarmBindClaimNudge_WaitsForIdleBeforeDelivering(t *testing.T) {
	sp := runtime.NewFake()
	session := warmBindPoolSession()
	store := beads.NewMemStoreFrom(0, []beads.Bead{*session}, nil)

	deliverWarmBindClaimNudge(context.Background(), sp, store, session, warmClaimText, alwaysUnclaimed)

	sawWait, sawNudge := false, false
	for _, c := range sp.Calls {
		switch c.Method {
		case "WaitForIdle":
			if c.Name == "worker-1" {
				sawWait = true
			}
		case "Nudge":
			if !sawWait {
				t.Fatal("Nudge delivered before WaitForIdle (mid-turn risk)")
			}
			sawNudge = true
		}
	}
	if !sawWait || !sawNudge {
		t.Fatalf("want both WaitForIdle and Nudge; got wait=%v nudge=%v", sawWait, sawNudge)
	}
}

// A claimed trigger (probe returns false) is never nudged and never marked — the
// churn invariant that keeps the nudge invisible to a working slot.
func TestDeliverWarmBindClaimNudge_SkipsClaimedTrigger(t *testing.T) {
	sp := runtime.NewFake()
	session := warmBindPoolSession()
	store := beads.NewMemStoreFrom(0, []beads.Bead{*session}, nil)

	deliverWarmBindClaimNudge(context.Background(), sp, store, session, warmClaimText, neverUnclaimed)
	if n, _ := countNudges(sp); n != 0 {
		t.Fatalf("claimed trigger: got %d nudges, want 0", n)
	}
	if got := session.Metadata[warmBindNudgedForTriggerKey]; got != "" {
		t.Fatalf("claimed trigger: marker = %q, want empty", got)
	}
}

// A non-pool session is ignored entirely (named sessions carry no claim work).
func TestDeliverWarmBindClaimNudge_SkipsNonPool(t *testing.T) {
	sp := runtime.NewFake()
	session := warmBindPoolSession()
	delete(session.Metadata, "pool_managed")
	store := beads.NewMemStoreFrom(0, []beads.Bead{*session}, nil)

	deliverWarmBindClaimNudge(context.Background(), sp, store, session, warmClaimText, alwaysUnclaimed)
	if n, _ := countNudges(sp); n != 0 {
		t.Fatalf("non-pool: got %d nudges, want 0", n)
	}
}

// A slot with no bound trigger, no claim text, or a nil probe delivers nothing.
func TestDeliverWarmBindClaimNudge_NoopGuards(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{*warmBindPoolSession()}, nil)

	cases := map[string]func() (*runtime.Fake, *beads.Bead, warmClaimTriggerProbe, string){
		"no bound trigger": func() (*runtime.Fake, *beads.Bead, warmClaimTriggerProbe, string) {
			s := warmBindPoolSession()
			delete(s.Metadata, beadmeta.TriggerBeadIDMetadataKey)
			return runtime.NewFake(), s, alwaysUnclaimed, warmClaimText
		},
		"empty claim text": func() (*runtime.Fake, *beads.Bead, warmClaimTriggerProbe, string) {
			return runtime.NewFake(), warmBindPoolSession(), alwaysUnclaimed, "   "
		},
		"nil probe": func() (*runtime.Fake, *beads.Bead, warmClaimTriggerProbe, string) { //nolint:unparam // the always-nil probe IS this case
			return runtime.NewFake(), warmBindPoolSession(), nil, warmClaimText
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			sp, session, probe, text := build()
			deliverWarmBindClaimNudge(context.Background(), sp, store, session, text, probe)
			if n, _ := countNudges(sp); n != 0 {
				t.Fatalf("%s: got %d nudges, want 0", name, n)
			}
		})
	}
}

// A delivery that fails leaves the marker unset so a later tick retries; the
// unclaimed gate keeps that retry safe.
func TestDeliverWarmBindClaimNudge_NoMarkerOnDeliveryFailure(t *testing.T) {
	sp := runtime.NewFailFake() // every provider op errors, incl. Nudge
	session := warmBindPoolSession()
	store := beads.NewMemStoreFrom(0, []beads.Bead{*session}, nil)

	deliverWarmBindClaimNudge(context.Background(), sp, store, session, warmClaimText, alwaysUnclaimed)

	if n, _ := countNudges(sp); n != 1 {
		t.Fatalf("delivery failure: want exactly one Nudge attempt, got %d", n)
	}
	if got := session.Metadata[warmBindNudgedForTriggerKey]; got != "" {
		t.Fatalf("delivery failure: in-memory marker = %q, want empty (retry next tick)", got)
	}
	stored, _ := store.Get("s-1")
	if got := stored.Metadata[warmBindNudgedForTriggerKey]; got != "" {
		t.Fatalf("delivery failure: persisted marker = %q, want empty", got)
	}
}

// isUnclaimedTrigger: only an open bead not already assigned to this slot counts.
func TestIsUnclaimedTrigger(t *testing.T) {
	cases := []struct {
		name string
		w    beads.Bead
		want bool
	}{
		{"open unassigned", beads.Bead{Status: "open"}, true},
		{"open assigned elsewhere", beads.Bead{Status: "open", Assignee: "worker-9"}, true},
		{"open assigned to self", beads.Bead{Status: "open", Assignee: "worker-1"}, false},
		{"in_progress", beads.Bead{Status: "in_progress"}, false},
		{"closed", beads.Bead{Status: "closed"}, false},
		{"blocked", beads.Bead{Status: "blocked"}, false},
	}
	for _, tc := range cases {
		if got := isUnclaimedTrigger(tc.w, "worker-1"); got != tc.want {
			t.Errorf("%s: isUnclaimedTrigger = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The probe resolves the trigger from the store named by gc.trigger_bead_store_ref
// (rig:<name> → the cached rig store; empty → the city store) and reports its
// live claim state — the resolution the reverted poller's assignee-gated snapshot
// could not do.
func TestBuildWarmClaimTriggerProbe_ResolvesFromStoreRef(t *testing.T) {
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "c-1", Status: "open"},
	}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "w-1", Status: "open"},
	}, nil)
	rigStores := map[string]beads.Store{"webapp": rigStore}
	probe := buildWarmClaimTriggerProbe(cityStore, rigStores)

	rigSession := func(trigger, ref string) beads.Bead {
		return beads.Bead{Metadata: map[string]string{
			"session_name":                          "worker-1",
			beadmeta.TriggerBeadIDMetadataKey:       trigger,
			beadmeta.TriggerBeadStoreRefMetadataKey: ref,
		}}
	}

	// rig:<name> → rig store; w-1 is open → unclaimed.
	if !probe(rigSession("w-1", "rig:webapp")) {
		t.Fatal("rig-ref open trigger: want unclaimed=true")
	}
	// Claim it in the rig store → probe flips to false.
	claimed := "in_progress"
	if err := rigStore.Update("w-1", beads.UpdateOpts{Status: &claimed}); err != nil {
		t.Fatalf("update w-1: %v", err)
	}
	if probe(rigSession("w-1", "rig:webapp")) {
		t.Fatal("rig-ref claimed trigger: want unclaimed=false")
	}
	// Empty ref → city store; c-1 open → unclaimed.
	if !probe(rigSession("c-1", "")) {
		t.Fatal("empty-ref open trigger: want unclaimed=true (city store)")
	}
	// rig ref naming an unknown store → fail-closed (nil store → false).
	if probe(rigSession("w-1", "rig:ghost")) {
		t.Fatal("unknown rig store: want fail-closed unclaimed=false")
	}
	// Missing trigger bead → read error → fail-closed.
	if probe(rigSession("missing", "rig:webapp")) {
		t.Fatal("missing trigger bead: want fail-closed unclaimed=false")
	}
	// No bound trigger id → nothing to probe.
	if probe(rigSession("", "rig:webapp")) {
		t.Fatal("no trigger id: want unclaimed=false")
	}
}
