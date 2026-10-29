package main

import (
	"context"
	"errors"
	"io"
	"maps"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// Arm A21 and the close effect (CONTRACT v5.7 C3; C5c1).

// closeCase is one row on an atomic-close store, its entry reading
// liveness, desire and reason, its city work on the same store.
type closeCase struct {
	store beads.Store
	w     *World
	a     *allocDecision
	k     rowKey
	cfg   *config.City
	rec   *events.Fake
	bare  bool // run without the close's around: the effect alone holds
}

func newCloseCase(t *testing.T, liveness rowLiveness, desired desire, meta ...string) *closeCase {
	t.Helper()
	store := beads.NewAtomicCloseMemStore()
	if err := beads.StampOpenedStore(store, "MemStore", gate.Require, false, nil, nil); err != nil {
		t.Fatal(err)
	}
	base := []string{"template", "worker", "session_name", "s-close", "generation", "3", "instance_token", "tok-3", "pool_managed", "true", "pool_slot", "1"}
	b, err := store.Create(sessionRow("close", append(base, meta...)...))
	if err != nil {
		t.Fatal(err)
	}
	c := &closeCase{store: store, k: rowKeyOf(b.ID), cfg: &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{persistentWorker(), otherLane(), {Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(3)}}}, rec: events.NewFake()}
	c.w = &World{Now: gatherNow, Census: readCensus(t, gatherNow, censusLegs(rowLeg, store)), Mislabelled: map[rowKey]bool{}, CityPath: t.TempDir()}
	c.w.LegStores = map[string]beads.Store{rowLeg: store}
	c.w.WorkLegs = testWorkLegs(c.w.CityPath, c.cfg, store, nil)
	c.a = &allocDecision{Snapshot: &selectionSnapshot{Entries: map[rowKey]*selectionEntry{
		c.k: {Key: c.k, Liveness: liveness, Desired: desired, Template: "worker"},
	}}}
	return c
}

func (c *closeCase) entry() *selectionEntry { return c.a.Snapshot.Entries[c.k] }

func (c *closeCase) decide() intent {
	it, _ := decideRow(c.w, c.a, c.k)
	return it
}

// run decides the row and runs its close with sp as the env's provider.
func (c *closeCase) run(t *testing.T, sp runtime.Provider) settlement {
	t.Helper()
	it := c.decide()
	if it.Kind != intentClose {
		t.Fatalf("decideRow = (%q, %q), want a close", it.Kind, it.Reason)
	}
	w := *c.w
	w.Env = &reconcileEnv{SP: sp, Cfg: c.cfg}
	p := newEffectPass(&w, c.a)
	p.held.release = newCloseReleases(c.w, &strandedClaimAlerts{rec: c.rec}, io.Discard)
	p.held.release.cfg = c.cfg
	spec := effectSpecs[it.Kind]
	if c.bare {
		spec.around = nil
	}
	return runTx(context.Background(), p, it, spec, nil)
}

func (c *closeCase) row(t *testing.T) beads.Bead {
	t.Helper()
	b, err := c.store.Get(c.k.ID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (c *closeCase) work(t *testing.T, status string, meta map[string]string) beads.Bead {
	t.Helper()
	return createWork(t, c.store, status, c.k.ID, meta)
}

// Kills a close proposed where A21 has none, and one that loses a kind's
// facts: each kind's reason, patch and spec on the rows A21 owns, nothing on
// the rows it must not touch (CONTRACT v5.7 A21, A10, B1; the C5c1 review).
func TestArmCloseDecides(t *testing.T) {
	at := func(d time.Duration) string { return gatherNow.Add(d).UTC().Format(time.RFC3339) }
	for _, tc := range []struct {
		name     string
		liveness rowLiveness
		desired  desire
		reason   string // the entry's reason
		drain    string
		meta     []string
		partial  bool
		// citySuspended is a city suspend.
		citySuspended bool
		suspendCause  string // the row's operator suspend cause (World.OperatorSuspend)
		want          string // decide reason, "" for none
		check         func(t *testing.T, it intent)
	}{
		{
			name: "a committed gone row AL1 drains as orphaned (A21 owns it, not A6)", liveness: livenessGone, desired: desireDrain, drain: drainOrphaned, meta: []string{"state", "active"},
			want: decideCloseOrphan, check: func(t *testing.T, it intent) {
				if it.Patch["state"] != "orphaned" || !it.Closing.Orphaned {
					t.Errorf("patch %v closing %+v, want orphaned", it.Patch, it.Closing)
				}
			},
		},
		{
			name: "an agent-suspended pool seat closes with its reason, releasing as legacy's orphan does", liveness: livenessGone, desired: desireDrain, drain: drainSuspended, meta: []string{"state", "asleep"},
			want: decideCloseOrphan, check: func(t *testing.T, it intent) {
				if it.Patch["state"] != "suspended" || !it.Closing.Orphaned {
					t.Errorf("patch %v closing %+v, want suspended and orphaned", it.Patch, it.Closing)
				}
			},
		},
		{
			name: "a suspended configured named row keeps its work", liveness: livenessGone, desired: desireDrain, drain: drainSuspended, meta: []string{"state", "asleep", "configured_named_session", "true", "configured_named_identity", "chat", "pool_managed", ""},
			want: decideCloseOrphan, check: func(t *testing.T, it intent) {
				if it.Closing.Orphaned {
					t.Errorf("closing %+v, want not orphaned", it.Closing)
				}
			},
		},
		{
			name: "a city suspend keeps a pool seat's work", liveness: livenessGone, desired: desireDrain, drain: drainSuspended, meta: []string{"state", "asleep"}, citySuspended: true,
			want: decideCloseOrphan, check: func(t *testing.T, it intent) {
				if it.Closing.Orphaned {
					t.Errorf("closing %+v, want not orphaned", it.Closing)
				}
			},
		},
		{name: "an undesired pending create is A10's rollback", liveness: livenessGone, desired: desireDrain, drain: drainOrphaned, meta: []string{"state", "creating", "pending_create_claim", "true"}},
		{
			name: "a failed create", liveness: livenessGone, desired: desireNone, reason: reasonFailedCreate, meta: []string{"state", "failed-create", "sleep_intent", "idle-stop-pending", "wait_hold", "true"},
			want: decideCloseFailedCreate, check: func(t *testing.T, it intent) {
				want := failedCreateCloseLife(gatherNow)
				want["sleep_intent"], want["wait_hold"] = "", "" // the Retire cell's clears
				if !maps.Equal(it.folded(), want) || !it.Class.Has("sleep_intent") {
					t.Errorf("patch %v, want failedCreateClosePatch's lifecycle with the Retire cell (%v)", it.Patch, want)
				}
			},
		},
		{name: "nothing on a partial pass", liveness: livenessGone, desired: desireNone, reason: reasonFailedCreate, meta: []string{"state", "failed-create"}, partial: true},
		{
			name: "an idle pool slot", liveness: livenessGone, desired: desireSleep, meta: []string{"state", "asleep", "sleep_reason", "idle", "slept_at", at(-time.Hour)},
			want: decideClosePoolSlot, check: func(t *testing.T, it intent) {
				if it.Patch["state"] != "idle" || it.Closing.Repair || it.Closing.Killed {
					t.Errorf("patch %v closing %+v, want an idle close", it.Patch, it.Closing)
				}
			},
		},
		{
			name: "a young stranded marker holds", liveness: livenessGone, desired: desireSleep, meta: []string{"state", "asleep", "sleep_reason", "idle", "slept_at", at(-time.Hour), strandedEventEmittedKey, at(-time.Minute)},
			want: decideStrandedHold,
		},
		{
			name: "an aged marker repairs", liveness: livenessGone, desired: desireSleep, meta: []string{"state", "asleep", "sleep_reason", "idle", "slept_at", at(-time.Hour), strandedEventEmittedKey, at(-time.Hour)},
			want: decideClosePoolSlot, check: func(t *testing.T, it intent) {
				if !it.Closing.Repair {
					t.Errorf("closing %+v, want the repair", it.Closing)
				}
			},
		},
		{
			name: "an unparseable marker never holds", liveness: livenessGone, desired: desireSleep, meta: []string{"state", "asleep", "sleep_reason", "idle", "slept_at", at(-time.Hour), strandedEventEmittedKey, "soon"},
			want: decideClosePoolSlot,
		},
		{
			name: "a killed seat ignores its marker (B1)", liveness: livenessGone, desired: desireSleep, meta: []string{"state", "asleep", "sleep_reason", "killed", "slept_at", at(-time.Hour), strandedEventEmittedKey, at(-time.Minute)},
			want: decideClosePoolSlot, check: func(t *testing.T, it intent) {
				if !it.Closing.Killed || it.Closing.Repair {
					t.Errorf("closing %+v, want killed, no repair", it.Closing)
				}
			},
		},
		{name: "a user-held slot keeps it", liveness: livenessGone, desired: desireSleep, meta: []string{"state", "asleep", "sleep_reason", "idle", "slept_at", at(-time.Hour), "sleep_intent", "user-hold"}},
		{name: "a desired floor seat asleep for no wake reason keeps its slot", liveness: livenessGone, desired: desireSleep, meta: []string{"state", "asleep", "sleep_reason", "no-wake-reason", "slept_at", at(-time.Hour)}},
		{
			// Unlike no-wake-reason (a genuine warm-standby: zero demand, cheap
			// re-wake with unchanged config), a config-drifted seat is never
			// woken in place (that would run stale config), so it must free its
			// slot for a fresh seat against the current config — otherwise a
			// pool whose every slot drifts goes dark forever (srvcity sr-sxbc7:
			// st/support.dispatch dark 17.5h, both slots asleep-for-drift, 59
			// ready routed steps queued, nothing closed or replaced it).
			name: "a desired seat asleep for config drift frees its slot (srvcity sr-sxbc7)", liveness: livenessGone, desired: desireSleep, meta: []string{"state", "asleep", "sleep_reason", "config-drift", "slept_at", at(-time.Hour)},
			want: decideClosePoolSlot, check: func(t *testing.T, it intent) {
				if it.Patch["state"] != "config-drift" || it.Closing.Repair || it.Closing.Killed {
					t.Errorf("patch %v closing %+v, want a config-drift close, no repair, not killed", it.Patch, it.Closing)
				}
			},
		},
		{name: "an undesired pool row under a user hold keeps its slot (§12.2 #27)", liveness: livenessGone, desired: desireDrain, drain: drainOrphaned, meta: []string{"state", "asleep", "sleep_intent", "user-hold"}},
		{name: "an undesired pool row under a wait hold keeps its slot", liveness: livenessGone, desired: desireDrain, drain: drainOrphaned, meta: []string{"state", "asleep", "wait_hold", "w-1"}},
		{name: "an undesired pool row under a quarantine keeps its slot", liveness: livenessGone, desired: desireDrain, drain: drainOrphaned, meta: []string{"state", "asleep", "quarantined_until", at(time.Hour)}},
		{name: "an undesired pool row under a hold timer keeps its slot", liveness: livenessGone, desired: desireDrain, drain: drainOrphaned, meta: []string{"state", "suspended", "held_until", at(time.Hour)}},
		// Any row, not only a pool row: an orphan close never ends an
		// operator's hold (the A14 generator's seed 660, mc-i7efm).
		{name: "an undesired manual row under an operator's suspend is kept", liveness: livenessGone, desired: desireDrain, drain: drainOrphaned, meta: []string{"pool_managed", "", "pool_slot", "", "session_origin", "manual", "state", "suspended", "sleep_intent", "user-hold", "held_until", at(1000 * time.Hour), "suspended_at", at(-time.Minute)}},
		{name: "an undesired manual row under a hold timer is kept", liveness: livenessGone, desired: desireDrain, drain: drainOrphaned, meta: []string{"pool_managed", "", "pool_slot", "", "session_origin", "manual", "state", "asleep", "held_until", at(time.Hour)}},
		{name: "an undesired manual row under no hold closes", liveness: livenessGone, desired: desireDrain, drain: drainOrphaned, meta: []string{"pool_managed", "", "pool_slot", "", "session_origin", "manual", "state", "asleep"}, want: decideCloseOrphan},
		// v5.10s: a non-pool row keeps only an operator's dormancy or a live,
		// readable hold timer; a wait hold or an unreadable timer closes, as
		// legacy's orphan close does.
		{name: "an undesired manual row under a wait hold closes", liveness: livenessGone, desired: desireDrain, drain: drainOrphaned, meta: []string{"pool_managed", "", "pool_slot", "", "session_origin", "manual", "state", "asleep", "wait_hold", "w-1", "sleep_intent", "wait-hold"}, want: decideCloseOrphan},
		{name: "an undesired manual row under an unreadable hold timer closes", liveness: livenessGone, desired: desireDrain, drain: drainOrphaned, meta: []string{"pool_managed", "", "pool_slot", "", "session_origin", "manual", "state", "asleep", "held_until", "not-a-time"}, want: decideCloseOrphan},
		// M4: a held manual row whose claimed runtime died is kept, so A12
		// classifies its dead runtime (no orphan close takes it).
		{name: "an undesired manual row under an operator's suspend with a dead runtime is classified", liveness: livenessDead, desired: desireDrain, drain: drainOrphaned, meta: []string{"pool_managed", "", "pool_slot", "", "session_origin", "manual", "state", "active", "sleep_intent", "user-hold", "suspended_at", at(-time.Hour)}, want: decideZombie},
		{name: "an undesired soft-suspended pool row heals asleep first (v5.9u)", liveness: livenessGone, desired: desireDrain, drain: drainOrphaned, meta: []string{"state", "suspended", "suspended_at", at(-time.Minute)}, want: decideSuspendHeal},
		{name: "and once healed, with its suspended_at left, closes", liveness: livenessGone, desired: desireDrain, drain: drainOrphaned, meta: []string{"state", "asleep", "suspended_at", at(-time.Minute)}, want: decideCloseOrphan},
		{
			name: "a future-dated marker counts as aged (§12.2 #24)", liveness: livenessGone, desired: desireSleep, meta: []string{"state", "asleep", "sleep_reason", "idle", "slept_at", at(-time.Hour), strandedEventEmittedKey, at(time.Hour)},
			want: decideClosePoolSlot, check: func(t *testing.T, it intent) {
				if !it.Closing.Repair {
					t.Errorf("closing %+v, want the repair", it.Closing)
				}
			},
		},
		{name: "a live runtime", liveness: livenessAlive, desired: desireDrain, drain: drainOrphaned, meta: []string{"state", "asleep"}},
		{name: "a dead row that claims a live runtime closes (MAINT-031, C5c2)", liveness: livenessDead, desired: desireDrain, drain: drainOrphaned, meta: []string{"state", "active"}, want: decideCloseOrphan},
		{name: "a dead row woken inside INC-003's grace waits it out", liveness: livenessDead, desired: desireDrain, drain: drainOrphaned, meta: []string{"state", "active", "last_woke_at", at(-time.Minute)}},
		{name: "an operator-suspended dead row gets no grace", liveness: livenessDead, desired: desireDrain, drain: drainOrphaned, meta: []string{"state", "active", "last_woke_at", at(-time.Minute)}, suspendCause: "agent", want: decideCloseOrphan},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCloseCase(t, tc.liveness, tc.desired, tc.meta...)
			e := c.entry()
			e.Reason, e.DrainReason = tc.reason, tc.drain
			if tc.partial {
				// A suspended city overwrites the partial mode; the global
				// cause still holds the close (SESS-060).
				c.a.Snapshot.Mode, c.a.Snapshot.Partial.Global = modeSuspended, []string{causeStoreQueryPartial}
			}
			c.w.CitySuspended = tc.citySuspended
			if tc.suspendCause != "" {
				c.w.OperatorSuspend = map[rowKey]string{c.k: tc.suspendCause}
			}
			it := c.decide()
			if it.Reason != tc.want && (tc.want != "" || it.Kind == intentClose) {
				t.Fatalf("decideRow = (%q, %q), want %q", it.Kind, it.Reason, tc.want)
			}
			if tc.want == decideStrandedHold && it.Kind != "" {
				t.Fatalf("stranded hold has kind %q, want a hold", it.Kind)
			}
			if tc.check != nil {
				tc.check(t, it)
			}
		})
	}
}

// Kills a SESS-080 phantom closed without its preserve set: the dead squatter
// of a configured name closes, and its kind keeps the identity's two forms.
func TestArmCloseKeepsAPhantomsIdentity(t *testing.T) {
	c := newCloseCase(t, livenessGone, desireDrain, "template", "keeper", "session_name", "chat", "configured_named_session", "true", "state", "asleep", "pool_managed", "")
	c.entry().DrainReason = drainOrphaned
	c.cfg.Agents = append(c.cfg.Agents, config.Agent{Name: "chat", StartCommand: "true"})
	c.cfg.NamedSessions = []config.NamedSession{{Template: "chat", Mode: "always"}}
	c.w.Phantoms = censusPhantoms(c.w.Census, c.cfg, "test-city")
	it := c.decide()
	if it.Reason != decideCloseOrphan || !it.Closing.Phantom || len(it.Closing.Preserve) == 0 || it.Closing.Preserve[0] != "chat" {
		t.Fatalf("decideRow = %q closing %+v, want a phantom close keeping chat", it.Reason, it.Closing)
	}
}

// The close effect, end to end on a gone runtime (C3 steps 2-5).
// Each case runs through the close's around and without it: the precheck
// saves a lease, and the effect alone keeps every claim it must (B1, L5).
func TestCloseEffect(t *testing.T) {
	for _, bare := range []bool{false, true} {
		t.Run(map[bool]string{false: "around", true: "bare"}[bare], func(t *testing.T) { closeEffectCases(t, bare) })
	}
}

func closeEffectCases(t *testing.T, bare bool) {
	mk := func(t *testing.T, liveness rowLiveness, desired desire, meta ...string) *closeCase {
		c := newCloseCase(t, liveness, desired, meta...)
		c.bare = bare
		return c
	}
	t.Run("a committed gone orphan closes, and its cascade runs", func(t *testing.T) {
		c := mk(t, livenessGone, desireDrain, "state", "active")
		c.entry().DrainReason = drainOrphaned
		s := c.run(t, gone().tmux())
		if s.Outcome != settledLanded || !s.Closed {
			t.Fatalf("settlement %+v, want the close landed", s)
		}
		if b := c.row(t); b.Status != "closed" || b.Metadata["state"] != "orphaned" || b.Metadata[session.RuntimeLeaseHolderKey] != "" {
			t.Fatalf("row %s %v, want closed orphaned with its lease cleared", b.Status, b.Metadata)
		}
	})
	t.Run("a failed create closes with its holds cleared (the Retire cell)", func(t *testing.T) {
		c := mk(t, livenessGone, desireNone, "state", "failed-create", "sleep_intent", "idle-stop-pending", "wait_hold", "true", "quarantined_until", gatherNow.Add(time.Hour).Format(time.RFC3339))
		c.entry().Reason = reasonFailedCreate
		if s := c.run(t, gone().tmux()); s.Outcome != settledLanded || !s.Closed {
			t.Fatalf("settlement %+v, want the close landed", s)
		}
		b := c.row(t)
		if b.Status != "closed" || b.Metadata["sleep_intent"] != "" || b.Metadata["wait_hold"] != "" || b.Metadata["quarantined_until"] != "" {
			t.Fatalf("row %s %v, want closed with every hold cleared", b.Status, b.Metadata)
		}
	})
	t.Run("a wake or restart request on the row holds no close", func(t *testing.T) {
		for _, req := range [][]string{{"wake_request", "explicit"}, {"restart_requested", "true"}} {
			c := mk(t, livenessGone, desireDrain, "state", "asleep", req[0], req[1])
			c.entry().DrainReason = drainOrphaned
			if s := c.run(t, gone().tmux()); s.Outcome != settledLanded || !s.Closed {
				t.Fatalf("%s: settlement %+v, want the close landed", req[0], s)
			}
		}
	})
	t.Run("work holds the close, with no lease taken", func(t *testing.T) {
		c := mk(t, livenessGone, desireDrain, "state", "asleep")
		c.entry().DrainReason, c.w.CitySuspended = drainSuspended, true
		c.work(t, "in_progress", routedClaim())
		before := c.row(t).Metadata[session.RuntimeLeaseEpochKey]
		s := c.run(t, gone().tmux())
		if s.Outcome != settledRefused || s.Cause != causeHasWork {
			t.Fatalf("settlement %+v, want refused has-work", s)
		}
		if b := c.row(t); b.Status == "closed" || !bare && b.Metadata[session.RuntimeLeaseEpochKey] != before {
			t.Fatalf("row %s epoch %q (was %q), want open and unleased", b.Status, b.Metadata[session.RuntimeLeaseEpochKey], before)
		}
	})
	t.Run("an orphaned orphan releases its served claim, then closes (SESS-082)", func(t *testing.T) {
		c := mk(t, livenessGone, desireDrain, "state", "asleep")
		c.entry().DrainReason = drainOrphaned
		claim := c.work(t, "in_progress", routedClaim())
		s := c.run(t, gone().tmux())
		if s.Outcome != settledLanded || len(s.Facts.Events) == 0 || s.Facts.Events[0].Type != events.BeadDeadAssigneeReopened {
			t.Fatalf("settlement %+v, want landed with bead.dead_assignee_reopened", s)
		}
		if got, _ := c.store.Get(claim.ID); got.Assignee != "" {
			t.Fatalf("claim assignee %q, want released", got.Assignee)
		}
	})
	t.Run("an orphaned orphan closes over a claim no lane serves, keeping it (O5)", func(t *testing.T) {
		c := mk(t, livenessGone, desireDrain, "state", "asleep")
		c.entry().DrainReason = drainOrphaned
		claim := c.work(t, "in_progress", map[string]string{beadmeta.RoutedToMetadataKey: "nowhere/lane"})
		s := c.run(t, gone().tmux())
		if s.Outcome != settledLanded || c.row(t).Status != "closed" {
			t.Fatalf("settlement %+v, want the close landed", s)
		}
		got, _ := c.store.Get(claim.ID)
		if got.Assignee != c.k.ID {
			t.Fatalf("claim assignee %q, want kept", got.Assignee)
		}
		if bare {
			return // the stamp and alert are the around's cascade
		}
		if got.Metadata[beadmeta.StrandedSessionMetadataKey] != c.k.ID {
			t.Fatalf("claim stamp %q, want stamped", got.Metadata[beadmeta.StrandedSessionMetadataKey])
		}
		if evts, _ := c.rec.List(events.Filter{Type: events.SessionStrandedClaims}); len(evts) != 1 {
			t.Fatalf("session.stranded_claims = %d, want 1", len(evts))
		}
	})
	t.Run("a suspended orphan keeps a claim no lane serves and stays open", func(t *testing.T) {
		c := mk(t, livenessGone, desireDrain, "state", "asleep")
		c.entry().DrainReason, c.w.CitySuspended = drainSuspended, true
		claim := c.work(t, "in_progress", map[string]string{beadmeta.RoutedToMetadataKey: "nowhere/lane"})
		if s := c.run(t, gone().tmux()); s.Cause != causeHasWork || c.row(t).Status == "closed" {
			t.Fatalf("settlement %+v, want refused has-work: only an orphaned orphan closes over stranded work", s)
		}
		if got, _ := c.store.Get(claim.ID); got.Assignee != c.k.ID {
			t.Fatalf("claim assignee %q, want kept", got.Assignee)
		}
	})
	t.Run("an unmarked pool slot with work is stamped stranded (SESS-624)", func(t *testing.T) {
		c := mk(t, livenessGone, desireSleep, "state", "asleep", "sleep_reason", "idle", "slept_at", gatherNow.Add(-time.Hour).Format(time.RFC3339))
		c.work(t, "in_progress", routedClaim())
		s := c.run(t, gone().tmux())
		b := c.row(t)
		if s.Outcome != settledLanded || b.Status == "closed" || b.Metadata[strandedEventEmittedKey] == "" {
			t.Fatalf("settlement %+v row %s %v, want the marker stamped and the row open", s, b.Status, b.Metadata)
		}
		if len(s.Facts.Events) != 1 || s.Facts.Events[0].Type != events.SessionStranded {
			t.Fatalf("events %+v, want session.stranded", s.Facts.Events)
		}
	})
	t.Run("an aged marker unclaims, then closes stranded-repair (SESS-625)", func(t *testing.T) {
		c := mk(t, livenessGone, desireSleep, "state", "asleep", "sleep_reason", "idle", "slept_at", gatherNow.Add(-time.Hour).Format(time.RFC3339), strandedEventEmittedKey, gatherNow.Add(-time.Hour).Format(time.RFC3339))
		claim := c.work(t, "in_progress", routedClaim())
		s := c.run(t, gone().tmux())
		if b := c.row(t); s.Outcome != settledLanded || b.Metadata["state"] != strandedRepairCloseReason {
			t.Fatalf("settlement %+v row %v, want closed stranded-repair", s, b.Metadata)
		}
		if got, _ := c.store.Get(claim.ID); got.Assignee != "" {
			t.Fatalf("claim assignee %q, want released", got.Assignee)
		}
	})
	t.Run("a killed seat gives back its unexecuted claim, then closes (B1)", func(t *testing.T) {
		c := mk(t, livenessGone, desireSleep, "state", "asleep", "sleep_reason", "killed", "slept_at", gatherNow.Add(-time.Hour).Format(time.RFC3339))
		claim := c.work(t, "open", routedClaim())
		s := c.run(t, gone().tmux())
		if s.Outcome != settledLanded || c.row(t).Status != "closed" {
			t.Fatalf("settlement %+v, want the close landed", s)
		}
		if got, _ := c.store.Get(claim.ID); got.Assignee != "" {
			t.Fatalf("claim assignee %q, want released", got.Assignee)
		}
	})
	t.Run("a killed seat holding started work keeps it, unleased (B1)", func(t *testing.T) {
		c := mk(t, livenessGone, desireSleep, "state", "asleep", "sleep_reason", "killed", "slept_at", gatherNow.Add(-time.Hour).Format(time.RFC3339))
		started := c.work(t, "in_progress", routedClaim())
		before := c.row(t).Metadata[session.RuntimeLeaseEpochKey]
		s := c.run(t, gone().tmux())
		if s.Cause != causeHasWork || c.row(t).Status == "closed" || !bare && c.row(t).Metadata[session.RuntimeLeaseEpochKey] != before {
			t.Fatalf("settlement %+v, want refused has-work", s)
		}
		if got, _ := c.store.Get(started.ID); got.Assignee != c.k.ID {
			t.Fatalf("started claim assignee %q, want kept", got.Assignee)
		}
	})
	t.Run("a killed seat keeps an open claim its row names current (B1)", func(t *testing.T) {
		c := mk(t, livenessGone, desireSleep, "state", "asleep", "sleep_reason", "killed", "slept_at", gatherNow.Add(-time.Hour).Format(time.RFC3339))
		claim := killedSeatNamesClaim(t, c)
		before := c.row(t).Metadata[session.RuntimeLeaseEpochKey]
		if s := c.run(t, gone().tmux()); s.Cause != causeHasWork || c.row(t).Status == "closed" || !bare && c.row(t).Metadata[session.RuntimeLeaseEpochKey] != before {
			t.Fatalf("settlement %+v, want refused has-work (with the around, unleased)", s)
		}
		if got, _ := c.store.Get(claim.ID); got.Assignee != c.k.ID {
			t.Fatalf("named claim assignee %q, want kept", got.Assignee)
		}
	})
	t.Run("a live pane refuses", func(t *testing.T) {
		c := mk(t, livenessGone, desireDrain, "state", "asleep")
		c.entry().DrainReason = drainOrphaned
		live := &freshObserver{Fake: runtime.NewFake(), l: runtime.Liveness{Running: true, Alive: true, ObjectID: "$1", ObjectCreated: "1"}, env: map[string]string{"GC_SESSION_ID": c.k.ID, "GC_INSTANCE_TOKEN": "tok-3"}}
		s := c.run(t, live.tmux())
		if s.Outcome != settledRefused || c.row(t).Status == "closed" {
			t.Fatalf("settlement %+v, want refused", s)
		}
	})
}

// killedSeatNamesClaim gives c's row an open claim it names as its current
// bead: B1's execution evidence beyond in_progress.
func killedSeatNamesClaim(t *testing.T, c *closeCase) beads.Bead {
	t.Helper()
	claim := c.work(t, "open", routedClaim())
	if err := c.store.SetMetadataBatch(c.k.ID, map[string]string{session.CurrentBeadIDKey: claim.ID}); err != nil {
		t.Fatal(err)
	}
	c.w.Census = readCensus(t, gatherNow, censusLegs(rowLeg, c.store))
	return claim
}

// flakyListStore fails List while fail is set.
type flakyListStore struct {
	beads.Store
	fail *atomic.Bool
}

// ConditionalWritesResolveTarget keeps the inner store's fenced writes.
func (s flakyListStore) ConditionalWritesResolveTarget() beads.Store { return s.Store }

func (s flakyListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.fail.Load() {
		return nil, errors.New("transient list failure")
	}
	return s.Store.List(q)
}

// corpseRead is a fresh read that finds a corpse of the close case's row;
// moving makes every read find a different object.
func corpseRead(c *closeCase, attached, moving bool) *freshObserver {
	f := &freshObserver{Fake: runtime.NewFake(), l: runtime.Liveness{Corpse: true, ObjectID: "$1", ObjectCreated: "1"}, env: map[string]string{"GC_SESSION_ID": c.k.ID, "GC_INSTANCE_TOKEN": "tok-3"}}
	if attached {
		f.Fake.SetAttached("s-close", true)
	}
	if moving {
		n := 0
		f.read = func() { n++; f.l.ObjectID = "$" + string(rune('0'+n)) }
	}
	return f
}

// C8.8's corpse branch, the cause a live read refuses with, and the work
// branches the end-to-end cases above do not reach.
func TestCloseEffectEdges(t *testing.T) {
	orphan := func(t *testing.T, meta ...string) *closeCase {
		c := newCloseCase(t, livenessGone, desireDrain, append([]string{"state", "asleep"}, meta...)...)
		c.entry().DrainReason = drainOrphaned
		return c
	}
	t.Run("a corpse is removed by object, then the row closes", func(t *testing.T) {
		c := orphan(t)
		f := corpseRead(c, false, false)
		if s := c.run(t, f.tmux()); s.Outcome != settledLanded || f.Fake.CountCalls("KillCorpseObject", "s-close") != 1 {
			t.Fatalf("settlement %+v, kills %d; want the corpse killed once and the close landed", s, f.Fake.CountCalls("KillCorpseObject", "s-close"))
		}
	})
	t.Run("an attached corpse stays, and counts as stopped", func(t *testing.T) {
		c := orphan(t)
		f := corpseRead(c, true, false)
		if s := c.run(t, f.tmux()); s.Outcome != settledLanded || f.Fake.CountCalls("KillCorpseObject", "s-close") != 0 {
			t.Fatalf("settlement %+v, kills %d; want no kill and the close landed", s, f.Fake.CountCalls("KillCorpseObject", "s-close"))
		}
	})
	t.Run("a corpse read on two objects is not killed", func(t *testing.T) {
		c := orphan(t)
		f := corpseRead(c, false, true)
		c.run(t, f.tmux())
		if n := f.Fake.CountCalls("KillCorpseObject", "s-close"); n != 0 {
			t.Fatalf("kills %d, want none: the read did not pin one object", n)
		}
	})
	t.Run("an undesired row's dead runtime is removed, then the row closes (MAINT-031)", func(t *testing.T) {
		c := newCloseCase(t, livenessGone, desireDrain, "state", "active")
		c.entry().DrainReason = drainOrphaned
		f := corpseRead(c, false, false)
		if s := c.run(t, f.tmux()); !s.Closed || f.Fake.CountCalls("KillCorpseObject", "s-close") != 1 {
			t.Fatalf("settlement %+v kills %d, want the corpse removed and the row closed", s, f.Fake.CountCalls("KillCorpseObject", "s-close"))
		}
	})
	t.Run("an undesired row's zombie is removed by object and recorded stopped, then the row closes", func(t *testing.T) {
		for _, tc := range []struct {
			name             string
			attached, absent bool // absent: the kill finds no zombie (another object)
			kills, stopped   int
			closed           bool
		}{
			{"removed", false, false, 1, 1, true},
			{"attached: refuses live-pane, nothing stopped", true, false, 0, 0, false},
			{"the kill finds no zombie: nothing recorded stopped", false, true, 1, 0, false},
			{"another row's zombie by the fresh read: never killed", false, false, 0, 0, false},
		} {
			c := newCloseCase(t, livenessDead, desireDrain, "state", "active")
			c.entry().DrainReason = drainOrphaned
			f := corpseRead(c, tc.attached, false)
			f.l = runtime.Liveness{Running: true, ObjectID: "$1", ObjectCreated: "1", PanePID: "42"}
			if err := f.Fake.Start(context.Background(), "s-close", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			f.Fake.Zombies["s-close"] = !tc.absent
			if tc.kills == 0 && !tc.attached {
				f.env["GC_SESSION_ID"], f.env["GC_INSTANCE_TOKEN"] = "gc-other", "theirs"
			}
			f.read = func() { // the kill removes it
				if f.Fake.CountCalls("KillZombieObject", "s-close") > 0 && !tc.absent {
					f.l = runtime.Liveness{}
				}
			}
			s := c.run(t, f.tmux())
			stopped := 0
			for _, e := range s.Facts.Events {
				if e.Type == events.SessionStopped && e.Subject == "worker" && e.SessionID == c.k.ID {
					stopped++
				}
			}
			if kills := f.Fake.CountCalls("KillZombieObject", "s-close"); kills != tc.kills || stopped != tc.stopped || s.Closed != tc.closed {
				t.Fatalf("%s: settlement %+v kills %d session.stopped %d, want %d %d closed %v", tc.name, s, kills, stopped, tc.kills, tc.stopped, tc.closed)
			}
			if tc.kills == 0 && s.Cause != causeLivePane {
				t.Fatalf("%s: settlement %+v, want refused live-pane", tc.name, s)
			}
		}
	})
	t.Run("a dead row inside INC-003's grace requeues at its end", func(t *testing.T) {
		woke := gatherNow.Add(-time.Minute)
		c := newCloseCase(t, livenessDead, desireDrain, "state", "active", "last_woke_at", woke.Format(time.RFC3339))
		c.entry().DrainReason = drainOrphaned
		if it, next := decideRow(c.w, c.a, c.k); it.Kind == intentClose || !next.Equal(woke.Add(wakeUndesiredGrace)) {
			t.Fatalf("decideRow = (%q, %v), want no close and a requeue at %v", it.Kind, next, woke.Add(wakeUndesiredGrace))
		}
	})
	t.Run("legacy's exclusions keep a live-claiming row's dead runtime off A21", func(t *testing.T) {
		for name, meta := range map[string][]string{
			"named": {"configured_named_session", "true", "configured_named_identity", "worker"},
			// A wanted seat the start parked: AL1's pool count skips a row
			// with a terminal error, so it reads Drain (orphaned), and must
			// never close as orphaned (§12.2 #19).
			"a wanted seat parked": {"provider_terminal_error", "model_not_found", "sleep_reason", string(session.SleepReasonProviderTerminalError)},
			"stop requested":       {session.DrainIntentReasonKey, "orphaned", session.DrainIntentAtKey, gatherNow.Format(time.RFC3339), session.DrainIntentIncarnationKey, "3"},
		} {
			c := newCloseCase(t, livenessDead, desireDrain, append([]string{"state", "active"}, meta...)...)
			c.entry().DrainReason = drainOrphaned
			c.w.Classified = map[rowKey]classifiedRuntime{c.k: {Token: deadToken(c.w.Observed[c.k].Identity)}} // A12 is done with it
			if it := c.decide(); it.Kind == intentClose {
				t.Fatalf("%s: decide proposed %q, want no close", name, it.Reason)
			}
		}
		for name, meta := range map[string][]string{
			"control":                    nil,
			"a stale terminal mark only": {"provider_terminal_error", "quota_exceeded"}, // nothing clears it: the park is the sleep reason
		} {
			c := newCloseCase(t, livenessDead, desireDrain, append([]string{"state", "active"}, meta...)...)
			c.entry().DrainReason = drainOrphaned
			c.w.Classified = map[rowKey]classifiedRuntime{c.k: {Token: deadToken(c.w.Observed[c.k].Identity)}}
			if it := c.decide(); it.Kind != intentClose {
				t.Fatalf("%s: decide = (%q, %q), want the close", name, it.Kind, it.Reason)
			}
		}
	})
	t.Run("a live pane refuses live-pane", func(t *testing.T) {
		c := orphan(t)
		live := &freshObserver{Fake: runtime.NewFake(), l: runtime.Liveness{Running: true, Alive: true, ObjectID: "$1", ObjectCreated: "1"}, env: map[string]string{"GC_SESSION_ID": c.k.ID, "GC_INSTANCE_TOKEN": "tok-3"}}
		if s := c.run(t, live.tmux()); s.Cause != causeLivePane {
			t.Fatalf("settlement %+v, want refused live-pane", s)
		}
	})
	t.Run("a phantom skips L5 and keeps the identity's work (SESS-080)", func(t *testing.T) {
		c := newCloseCase(t, livenessGone, desireDrain, "template", "keeper", "session_name", "chat", "configured_named_session", "true", "state", "asleep", "pool_managed", "")
		c.entry().DrainReason = drainOrphaned
		c.cfg.Agents = append(c.cfg.Agents, config.Agent{Name: "chat", StartCommand: "true"})
		c.cfg.NamedSessions = []config.NamedSession{{Template: "chat", Mode: "always"}}
		c.w.Phantoms = censusPhantoms(c.w.Census, c.cfg, "test-city")
		kept := createWork(t, c.store, "open", "chat", routedClaim()) // served: no lane rule keeps it
		if s := c.run(t, gone().tmux()); s.Outcome != settledLanded {
			t.Fatalf("settlement %+v, want the phantom closed over the identity's work", s)
		}
		if got, _ := c.store.Get(kept.ID); got.Assignee != "chat" {
			t.Fatalf("identity's claim assignee %q, want kept for the canonical row", got.Assignee)
		}
	})
	t.Run("a repair whose release fails defers (SESS-625)", func(t *testing.T) {
		at := gatherNow.Add(-time.Hour).Format(time.RFC3339)
		c := newCloseCase(t, livenessGone, desireSleep, "state", "asleep", "sleep_reason", "idle", "slept_at", at, strandedEventEmittedKey, at)
		claim := c.work(t, "in_progress", routedClaim())
		c.w.WorkLegs = testWorkLegs(c.w.CityPath, c.cfg, releaseFailsStore{Store: c.store, id: claim.ID}, nil)
		if s := c.run(t, gone().tmux()); s.Cause != causeReleaseFailed || c.row(t).Status == "closed" {
			t.Fatalf("settlement %+v, want refused release-failed and the row open", s)
		}
	})
	t.Run("a precheck read failure runs the effect, which keeps a named claim (B1)", func(t *testing.T) {
		c := newCloseCase(t, livenessGone, desireSleep, "state", "asleep", "sleep_reason", "killed", "slept_at", gatherNow.Add(-time.Hour).Format(time.RFC3339))
		claim := killedSeatNamesClaim(t, c)
		var fail atomic.Bool
		fail.Store(true)
		c.w.WorkLegs = testWorkLegs(c.w.CityPath, c.cfg, flakyListStore{Store: c.store, fail: &fail}, nil)
		orig := withRowMutationLock
		withRowMutationLock = func(id string, fn func() error) error {
			fail.Store(false) // the effect's first section: the precheck is over
			return orig(id, fn)
		}
		t.Cleanup(func() { withRowMutationLock = orig })
		s := c.run(t, gone().tmux())
		if got, _ := c.store.Get(claim.ID); got.Assignee != c.k.ID || s.Cause != causeHasWork {
			t.Fatalf("settlement %+v claim assignee %q, want refused has-work with the claim kept", s, got.Assignee)
		}
	})
	t.Run("a detached probe alive keeps the slot unstamped (SESS-624)", func(t *testing.T) {
		installFakeTmuxServer(t, "gctest-c5c1", fakeTmuxListSessions("handoff"))
		c := newCloseCase(t, livenessGone, desireSleep, "state", "asleep", "sleep_reason", "idle", "slept_at", gatherNow.Add(-time.Hour).Format(time.RFC3339))
		c.work(t, "in_progress", detachedClaim())
		s := c.run(t, gone().tmux())
		if b := c.row(t); s.Cause != causeStranded || b.Metadata[strandedEventEmittedKey] != "" || len(s.Facts.Events) != 0 {
			t.Fatalf("settlement %+v marker %q, want refused stranded with no marker and no event", s, b.Metadata[strandedEventEmittedKey])
		}
	})
	t.Run("a detached probe dead stamps the slot (SESS-624)", func(t *testing.T) {
		installFakeTmuxServer(t, "gctest-c5c1", fakeTmuxListSessions())
		c := newCloseCase(t, livenessGone, desireSleep, "state", "asleep", "sleep_reason", "idle", "slept_at", gatherNow.Add(-time.Hour).Format(time.RFC3339))
		c.work(t, "in_progress", detachedClaim())
		if s := c.run(t, gone().tmux()); s.Outcome != settledLanded || c.row(t).Metadata[strandedEventEmittedKey] == "" {
			t.Fatalf("settlement %+v, want the marker stamped", s)
		}
	})
	t.Run("a detached probe that observed nothing leaves the slot unsearched and unstamped (AC-6)", func(t *testing.T) {
		for _, p := range []struct {
			name  string
			setup func(t *testing.T)
		}{
			{"error: no server socket", func(t *testing.T) {
				t.Setenv("TMUX_TMPDIR", shortTmuxTmpDir(t))
				installFakeTmux(t, "exit 1")
			}},
			{"error: the server does not answer", func(t *testing.T) { installFakeTmuxServer(t, "gctest-c5c1", "exit 2") }},
			{"timeout", func(t *testing.T) { installFakeTmuxServer(t, "gctest-c5c1", "sleep 2") }},
		} {
			t.Run(p.name, func(t *testing.T) {
				p.setup(t)
				k := newCloseCase(t, livenessGone, desireSleep, "state", "asleep", "sleep_reason", "idle", "slept_at", gatherNow.Add(-time.Hour).Format(time.RFC3339))
				k.work(t, "in_progress", detachedClaim())
				w := *k.w
				w.Env = &reconcileEnv{Cfg: k.cfg}
				v := txView{It: k.decide(), World: &w, Row: k.w.Census.Rows[k.k].Info, Fence: txFence{Work: &txWork{}}}
				got, err := strandedProbe(context.Background(), effectReads{legs: testWorkLegs(w.CityPath, k.cfg, k.store, nil)}, v)
				if err != nil || got.searched || len(got.ids) != 0 {
					t.Fatalf("probe = %+v, %v: want unsearched with nothing listed stranded", got, err)
				}
				s := k.run(t, gone().tmux())
				if b := k.row(t); s.Cause != causeStranded || b.Metadata[strandedEventEmittedKey] != "" || len(s.Facts.Events) != 0 {
					t.Fatalf("settlement %+v marker %q, want refused stranded with no marker and no event", s, b.Metadata[strandedEventEmittedKey])
				}
			})
		}
	})
	t.Run("an aged marker keeps a detached claim its probe does not attest dead, and the row open (SESS-625)", func(t *testing.T) {
		for _, p := range []struct {
			name  string
			setup func(t *testing.T)
		}{
			{"alive", func(t *testing.T) { installFakeTmuxServer(t, "gctest-c5c1", fakeTmuxListSessions("handoff")) }},
			{"error: no server socket", func(t *testing.T) {
				t.Setenv("TMUX_TMPDIR", shortTmuxTmpDir(t))
				installFakeTmux(t, "exit 1")
			}},
			{"error: the server does not answer", func(t *testing.T) { installFakeTmuxServer(t, "gctest-c5c1", "exit 2") }},
			{"timeout", func(t *testing.T) { installFakeTmuxServer(t, "gctest-c5c1", "sleep 2") }},
		} {
			t.Run(p.name, func(t *testing.T) {
				p.setup(t)
				at := gatherNow.Add(-time.Hour).Format(time.RFC3339)
				c := newCloseCase(t, livenessGone, desireSleep, "state", "asleep", "sleep_reason", "idle", "slept_at", at, strandedEventEmittedKey, at)
				claim := c.work(t, "in_progress", detachedClaim())
				s := c.run(t, gone().tmux())
				got, _ := c.store.Get(claim.ID)
				if s.Cause != causeDetachedHeld || c.row(t).Status == "closed" {
					t.Fatalf("settlement %+v row %s, want refused %s with the row open", s, c.row(t).Status, causeDetachedHeld)
				}
				if got.Assignee != c.k.ID || got.Metadata[beadmeta.DetachedMetadataKey] == "" {
					t.Fatalf("claim assignee %q gc.detached %q, want the held claim kept", got.Assignee, got.Metadata[beadmeta.DetachedMetadataKey])
				}
			})
		}
	})
	t.Run("an aged marker releases a dead detached claim, clearing gc.detached, then closes stranded-repair (SESS-625)", func(t *testing.T) {
		installFakeTmuxServer(t, "gctest-c5c1", fakeTmuxListSessions())
		at := gatherNow.Add(-time.Hour).Format(time.RFC3339)
		c := newCloseCase(t, livenessGone, desireSleep, "state", "asleep", "sleep_reason", "idle", "slept_at", at, strandedEventEmittedKey, at)
		claim := c.work(t, "in_progress", detachedClaim())
		s := c.run(t, gone().tmux())
		if b := c.row(t); s.Outcome != settledLanded || b.Metadata["state"] != strandedRepairCloseReason {
			t.Fatalf("settlement %+v row %v, want closed stranded-repair", s, b.Metadata)
		}
		if got, _ := c.store.Get(claim.ID); got.Assignee != "" || got.Metadata[beadmeta.DetachedMetadataKey] != "" {
			t.Fatalf("claim assignee %q gc.detached %q, want released with gc.detached cleared", got.Assignee, got.Metadata[beadmeta.DetachedMetadataKey])
		}
	})
	t.Run("an orphan whose served claim the release keeps does not close", func(t *testing.T) {
		installFakeTmuxServer(t, "gctest-c5c1", fakeTmuxListSessions("handoff")) // the claim's detached probe is alive
		c := orphan(t)
		claim := c.work(t, "in_progress", detachedClaim())
		s := c.run(t, gone().tmux())
		if got, _ := c.store.Get(claim.ID); c.row(t).Status == "closed" || got.Assignee != c.k.ID {
			t.Fatalf("settlement %+v row %s claim assignee %q, want the row open and the claim kept", s, c.row(t).Status, got.Assignee)
		}
	})
	t.Run("the cascade releases nothing on a row open again", func(t *testing.T) {
		c := orphan(t)
		claim := c.work(t, "in_progress", routedClaim())
		rel := newCloseReleases(c.w, &strandedClaimAlerts{rec: c.rec}, io.Discard)
		rel.cfg = c.cfg
		rel.cascade(context.Background(), c.decide()) // the row never closed
		if got, _ := c.store.Get(claim.ID); got.Assignee != c.k.ID {
			t.Fatalf("claim assignee %q, want kept: the cascade runs only on a closed row", got.Assignee)
		}
	})
	t.Run("a short budget never counts a live handoff stranded", func(t *testing.T) {
		installFakeTmuxServer(t, "gctest-c5c1", fakeTmuxListSessions("handoff")) // alive
		for _, c := range []struct {
			name     string
			blocked  bool // the read waits out the Probe's bound
			searched bool
		}{
			{"the probes run on their own bounds, however little the read left", false, true},
			{"a read that outlasts the Probe's bound leaves it unsearched", true, false},
		} {
			t.Run(c.name, func(t *testing.T) {
				k := newCloseCase(t, livenessGone, desireSleep, "state", "asleep", "sleep_reason", "idle", "slept_at", gatherNow.Add(-time.Hour).Format(time.RFC3339))
				k.work(t, "in_progress", detachedClaim())
				w := *k.w
				w.Env = &reconcileEnv{Cfg: k.cfg}
				ctx, cancel := context.WithTimeout(context.Background(), detachedProbeDefaultTimeout/2) // less than one probe's bound
				defer cancel()
				legs := testWorkLegs(w.CityPath, k.cfg, k.store, nil)
				if c.blocked {
					legs = testWorkLegs(w.CityPath, k.cfg, blockedListStore{Store: k.store, until: ctx.Done()}, nil)
				}
				v := txView{It: k.decide(), World: &w, Row: k.w.Census.Rows[k.k].Info, Fence: txFence{Work: &txWork{}}}
				got, err := strandedProbe(ctx, effectReads{legs: legs}, v)
				if err != nil || got.searched != c.searched || len(got.ids) != 0 || got.alive != c.searched {
					t.Fatalf("probe = %+v, %v: want searched %v, nothing counted stranded", got, err, c.searched)
				}
				if step := closeWorkStep(txView{It: v.It, World: &w, Alloc: k.a, Row: v.Row, Meta: k.row(t).Metadata, Fence: v.Fence}, got, nil); step.Refuse != causeStranded {
					t.Fatalf("step %+v, want refused stranded-unstamped", step)
				}
			})
		}
	})
	t.Run("a cascade past its deadline stamps what it leaves", func(t *testing.T) {
		c := orphan(t)
		claim := c.work(t, "in_progress", map[string]string{beadmeta.RunTargetMetadataKey: "repo/other"}) // the carried lane alone serves it
		it := c.decide()
		if err := c.store.Close(c.k.ID); err != nil {
			t.Fatal(err)
		}
		rel := newCloseReleases(c.w, &strandedClaimAlerts{rec: c.rec}, io.Discard)
		rel.cfg = c.cfg
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		rel.cascade(ctx, it)
		got, _ := c.store.Get(claim.ID)
		if got.Assignee != c.k.ID || got.Metadata[beadmeta.StrandedSessionMetadataKey] != c.k.ID {
			t.Fatalf("claim assignee %q stamp %v, want kept and stamped on the closed row", got.Assignee, got.Metadata)
		}
		if evts, _ := c.rec.List(events.Filter{Type: events.SessionStrandedClaims}); len(evts) != 1 {
			t.Fatalf("session.stranded_claims = %d, want the left claim alerted", len(evts))
		}
		// The detector's re-alert names it budget too, as the close did.
		rec := events.NewFake()
		detectStrandedClaims(c.w.WorkLegs, c.store, nil, &strandedClaimAlerts{rec: rec, scanned: gatherNow.Add(-strandedRealertEvery)}, gatherNow)
		evts, _ := rec.List(events.Filter{Type: events.SessionStrandedClaims})
		if len(evts) != 1 || !strings.Contains(string(evts[0].Payload), `"reason":"`+failedBudget+`"`) {
			t.Fatalf("re-alerts %+v, want one naming the claim %s", evts, failedBudget)
		}
	})
	t.Run("a cascade on a canceled context releases nothing and stamps what it leaves (D4)", func(t *testing.T) {
		c := orphan(t)
		claim := c.work(t, "in_progress", routedClaim())
		it := c.decide()
		if err := c.store.Close(c.k.ID); err != nil {
			t.Fatal(err)
		}
		rel := newCloseReleases(c.w, &strandedClaimAlerts{rec: c.rec}, io.Discard)
		rel.cfg = c.cfg
		ctx, cancel := context.WithCancel(context.Background()) // a shutdown: no deadline
		cancel()
		rel.cascade(ctx, it)
		if got, _ := c.store.Get(claim.ID); got.Assignee != c.k.ID || got.Metadata[beadmeta.StrandedSessionMetadataKey] != c.k.ID {
			t.Fatalf("claim assignee %q stamp %v, want kept and stamped", got.Assignee, got.Metadata)
		}
	})
	t.Run("past its deadline a cascade neither prunes nor clears the current claim (D9, D10)", func(t *testing.T) {
		pruned := 0
		orig := pruneWorktree
		pruneWorktree = func(session.Info, string, *config.City, io.Writer) { pruned++ }
		t.Cleanup(func() { pruneWorktree = orig })
		for _, past := range []bool{false, true} {
			c := newCloseCase(t, livenessGone, desireSleep, "state", "asleep", "sleep_reason", "idle", "slept_at", gatherNow.Add(-time.Hour).Format(time.RFC3339),
				"work_dir", "/city/.gc/worktrees/repo/worker-1", beadmeta.CurrentClaimBeadIDMetadataKey, "gw-x")
			it := c.decide()
			if err := c.store.Close(c.k.ID); err != nil {
				t.Fatal(err)
			}
			rel := newCloseReleases(c.w, &strandedClaimAlerts{rec: c.rec}, io.Discard)
			rel.cfg = c.cfg
			at := time.Now().Add(time.Minute)
			if past {
				at = time.Now().Add(-time.Second)
			}
			ctx, cancel := context.WithDeadline(context.Background(), at)
			rel.cascade(ctx, it)
			cancel()
			cleared := c.row(t).Metadata[beadmeta.CurrentClaimBeadIDMetadataKey] == ""
			if want := map[bool]int{false: 1, true: 1}[past]; pruned != want || cleared == past {
				t.Fatalf("past %v: pruned %d (want %d), current claim cleared %v", past, pruned, want, cleared)
			}
		}
	})
	t.Run("the prune spares a work dir a successor shares", func(t *testing.T) {
		dir := "/city/.gc/worktrees/repo/worker-1"
		for _, key := range []string{"work_dir", "worker_dir", beadmeta.WorkDirMetadataKey} {
			c := newCloseCase(t, livenessGone, desireSleep, "state", "asleep", "sleep_reason", "idle", "slept_at", gatherNow.Add(-time.Hour).Format(time.RFC3339), "work_dir", dir)
			rel := newCloseReleases(c.w, nil, io.Discard)
			if rel.workDirInUse(c.store, c.row(t)) {
				t.Fatalf("%s: in use with no successor", key)
			}
			if _, err := c.store.Create(sessionRow("succ", "template", "worker", "session_name", "s-succ", "pool_managed", "true", "pool_slot", "1", "state", "awake", key, dir+"/")); err != nil { // the same dir, uncleaned
				t.Fatal(err)
			}
			if !rel.workDirInUse(c.store, c.row(t)) {
				t.Fatalf("a successor recording the dir as %s reads not in use: the prune would remove its worktree", key)
			}
		}
	})
	t.Run("a killed seat's workflow root takes the seat's route (N08)", func(t *testing.T) {
		c := newCloseCase(t, livenessGone, desireSleep, "state", "asleep", "sleep_reason", "killed", "slept_at", gatherNow.Add(-time.Hour).Format(time.RFC3339))
		root := c.work(t, "open", map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow})
		if s := c.run(t, gone().tmux()); s.Outcome != settledLanded || c.row(t).Status != "closed" {
			t.Fatalf("settlement %+v, want the root released on the seat's route and the close landed", s)
		}
		if got, _ := c.store.Get(root.ID); got.Assignee != "" {
			t.Fatalf("root assignee %q, want released", got.Assignee)
		}
	})
	t.Run("the cascade clears the closed row's current claim (N09)", func(t *testing.T) {
		c := orphan(t, beadmeta.CurrentClaimBeadIDMetadataKey, "gw-x")
		if s := c.run(t, gone().tmux()); !s.Closed {
			t.Fatalf("settlement %+v, want closed", s)
		}
		if got := c.row(t).Metadata[beadmeta.CurrentClaimBeadIDMetadataKey]; got != "" {
			t.Fatalf("current claim %q, want cleared by the cascade", got)
		}
	})
	t.Run("an unparseable marker with work takes no lease", func(t *testing.T) {
		c := newCloseCase(t, livenessGone, desireSleep, "state", "asleep", "sleep_reason", "idle", "slept_at", gatherNow.Add(-time.Hour).Format(time.RFC3339), strandedEventEmittedKey, "soon")
		c.work(t, "in_progress", routedClaim())
		before := c.row(t).Metadata[session.RuntimeLeaseEpochKey]
		if s := c.run(t, gone().tmux()); s.Cause != causeHasWork || c.row(t).Metadata[session.RuntimeLeaseEpochKey] != before {
			t.Fatalf("settlement %+v epoch %q (was %q), want refused has-work with no lease", s, c.row(t).Metadata[session.RuntimeLeaseEpochKey], before)
		}
	})
}

// blockedListStore holds every List until until closes: a remote bd whose
// list outlasts its caller's bound.
type blockedListStore struct {
	beads.Store
	until <-chan struct{}
}

func (s blockedListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	<-s.until
	return s.Store.List(q)
}

// ConditionalWritesResolveTarget keeps the inner store's fenced writes.
func (s blockedListStore) ConditionalWritesResolveTarget() beads.Store { return s.Store }

// detachedClaim is a routed claim with a detached runtime installFakeTmuxServer
// answers for.
func detachedClaim() map[string]string {
	meta := routedClaim()
	meta[beadmeta.DetachedMetadataKey] = "tmux:gctest-c5c1:handoff"
	return meta
}

// L5's identity set by kind (O4): a close reads the narrow ReleaseScope, any
// other L5 reader the seat's RefuseScope, so a claim under a prior alias
// holds a drain but not a close.
func TestReadWorkScopeByKind(t *testing.T) {
	c := newCloseCase(t, livenessGone, desireDrain, "state", "asleep", "alias_history", "morsov", "pool_slot", "")
	createWork(t, c.store, "in_progress", "morsov", routedClaim())
	w := *c.w
	w.Env = &reconcileEnv{Cfg: c.cfg}
	p := newEffectPass(&w, c.a)
	row := c.w.Census.Rows[c.k].Info
	if got := p.readWork(row, intent{Kind: intentClose}); !got.Free {
		t.Fatalf("close's L5 = %+v, want free: a prior alias is not the dead seat's", got)
	}
	if got := p.readWork(row, intent{Kind: intentDrainBegin}); got.Free {
		t.Fatalf("a drain's L5 = %+v, want work: the live seat may hold a prior alias's claim", got)
	}
}

// Kills the close re-deciding on another row's zombie (the C5c2 delta
// review): on the fresh read only the row's own zombie is a dead runtime
// A21 closes over; another row's is a live pane, so no work branch runs
// ahead of the live-pane refusal.
func TestFreshClosableOwnZombieOnly(t *testing.T) {
	c := newCloseCase(t, livenessDead, desireDrain, "state", "active")
	c.entry().DrainReason = drainOrphaned
	row := c.w.Census.Rows[c.k]
	for name, id := range map[string]runtimeIdentity{
		"own":     {Known: true, SessionID: c.k.ID, Epoch: "3", Token: "tok-3"},
		"another": {Known: true, SessionID: "gc-other", Epoch: "1", Token: "theirs"},
	} {
		r := &rowFacts{w: c.w, k: c.k, row: row, found: true, entry: c.entry(), fresh: &txRuntime{Class: rtZombie, Same: true, Identity: id}}
		if got := r.freshClosable(); got != (name == "own") {
			t.Fatalf("%s zombie: freshClosable = %v", name, got)
		}
	}
}

// Kills a failed create closed under its still-leased claim (SESS-058,
// mc-mcud0): legacy keeps it open while the pending-create lease holds, a
// start possibly holding it; once the lease expires, or its agent is
// suspended, A21 closes it, and a lease anchor skewed ahead of the clock
// (v5.9r) holds nothing.
func TestFailedCreateHeldWhileLeased(t *testing.T) {
	ago := func(d time.Duration) string { return gatherNow.Add(d).Format(time.RFC3339) }
	for _, c := range []struct {
		name      string
		meta      []string
		suspended bool
		want      string
	}{
		{"leased (never started, inside the deadline)", []string{"pending_create_started_at", ago(-time.Minute)}, false, decideFailedCreateLeased},
		{"leased (a start in flight)", []string{"pending_create_started_at", ago(-11 * time.Minute), "last_woke_at", ago(-10 * time.Second)}, false, decideFailedCreateLeased},
		{"the lease expired", []string{"pending_create_started_at", ago(-11 * time.Minute)}, false, decideCloseFailedCreate},
		{"its agent suspended", []string{"pending_create_started_at", ago(-time.Minute)}, true, decideCloseFailedCreate},
		{"an anchor 6 minutes ahead", []string{"pending_create_started_at", ago(6 * time.Minute)}, false, decideCloseFailedCreate},
		{"an anchor a year ahead", []string{"pending_create_started_at", ago(365 * 24 * time.Hour)}, false, decideCloseFailedCreate},
	} {
		cc := newCloseCase(t, livenessGone, desireNone, append([]string{"state", "failed-create", "pending_create_claim", "true"}, c.meta...)...)
		cc.entry().Reason = reasonFailedCreate
		for i := range cc.cfg.Agents {
			if cc.cfg.Agents[i].Name == "worker" {
				cc.cfg.Agents[i].Suspended = c.suspended
			}
		}
		cc.w.Env = &reconcileEnv{Cfg: cc.cfg}
		if it := cc.decide(); it.Reason != c.want || (c.want == decideFailedCreateLeased) != (it.Kind == "") {
			t.Fatalf("%s: decideRow = (%q, %q), want %q", c.name, it.Kind, it.Reason, c.want)
		}
	}
}
