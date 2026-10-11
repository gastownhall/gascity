package main

import (
	"context"
	"io"
	"maps"
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
}

func newCloseCase(t *testing.T, liveness rowLiveness, desired desire, meta ...string) *closeCase {
	t.Helper()
	store := beads.NewAtomicCloseMemStore()
	if err := beads.StampOpenedStore(store, "MemStore", gate.Require, nil, nil); err != nil {
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
	return runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
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
		want     string // decide reason, "" for none
		check    func(t *testing.T, it intent)
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
			name: "a suspended drain closes with its reason", liveness: livenessGone, desired: desireDrain, drain: drainSuspended, meta: []string{"state", "asleep"},
			want: decideCloseOrphan, check: func(t *testing.T, it intent) {
				if it.Patch["state"] != "suspended" || it.Closing.Orphaned {
					t.Errorf("patch %v closing %+v, want suspended, not orphaned", it.Patch, it.Closing)
				}
			},
		},
		{name: "an undesired pending create is A10's rollback", liveness: livenessGone, desired: desireDrain, drain: drainOrphaned, meta: []string{"state", "creating", "pending_create_claim", "true"}},
		{
			name: "a failed create", liveness: livenessGone, desired: desireNone, reason: reasonFailedCreate, meta: []string{"state", "failed-create"},
			want: decideCloseFailedCreate, check: func(t *testing.T, it intent) {
				if !maps.Equal(it.Patch, failedCreateClosePatch(gatherNow)) {
					t.Errorf("patch %v, want failedCreateClosePatch", it.Patch)
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
		{
			name: "a wisp-tier suspended row frees its slot (A10)", liveness: livenessGone, desired: desireSleep, meta: []string{"state", "suspended", "session_origin", "ephemeral"},
			want: decideClosePoolSlot,
		},
		{name: "a held wisp-tier row keeps it (§12.2 #27)", liveness: livenessGone, desired: desireSleep, meta: []string{"state", "suspended", "session_origin", "ephemeral", "held_until", at(time.Hour)}},
		{name: "a live runtime", liveness: livenessAlive, desired: desireDrain, drain: drainOrphaned, meta: []string{"state", "asleep"}},
		{name: "a dead row that claims a live runtime is C5c2's", liveness: livenessDead, desired: desireDrain, drain: drainOrphaned, meta: []string{"state", "active"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCloseCase(t, tc.liveness, tc.desired, tc.meta...)
			e := c.entry()
			e.Reason, e.DrainReason = tc.reason, tc.drain
			if tc.partial {
				c.a.Snapshot.Mode = modePartial
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
func TestCloseEffect(t *testing.T) {
	t.Run("a committed gone orphan closes, and its cascade runs", func(t *testing.T) {
		c := newCloseCase(t, livenessGone, desireDrain, "state", "active")
		c.entry().DrainReason = drainOrphaned
		s := c.run(t, gone().tmux())
		if s.Outcome != settledLanded || !s.Closed {
			t.Fatalf("settlement %+v, want the close landed", s)
		}
		if b := c.row(t); b.Status != "closed" || b.Metadata["state"] != "orphaned" || b.Metadata[session.RuntimeLeaseHolderKey] != "" {
			t.Fatalf("row %s %v, want closed orphaned with its lease cleared", b.Status, b.Metadata)
		}
	})
	t.Run("work holds the close, with no lease taken", func(t *testing.T) {
		c := newCloseCase(t, livenessGone, desireDrain, "state", "asleep")
		c.entry().DrainReason = drainSuspended
		c.work(t, "in_progress", routedClaim())
		before := c.row(t).Metadata[session.RuntimeLeaseEpochKey]
		s := c.run(t, gone().tmux())
		if s.Outcome != settledRefused || s.Cause != causeHasWork {
			t.Fatalf("settlement %+v, want refused has-work", s)
		}
		if b := c.row(t); b.Status == "closed" || b.Metadata[session.RuntimeLeaseEpochKey] != before {
			t.Fatalf("row %s epoch %q (was %q), want open and unleased", b.Status, b.Metadata[session.RuntimeLeaseEpochKey], before)
		}
	})
	t.Run("an orphaned orphan releases its served claim, then closes (SESS-082)", func(t *testing.T) {
		c := newCloseCase(t, livenessGone, desireDrain, "state", "asleep")
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
		c := newCloseCase(t, livenessGone, desireDrain, "state", "asleep")
		c.entry().DrainReason = drainOrphaned
		claim := c.work(t, "in_progress", map[string]string{beadmeta.RoutedToMetadataKey: "nowhere/lane"})
		s := c.run(t, gone().tmux())
		if s.Outcome != settledLanded || c.row(t).Status != "closed" {
			t.Fatalf("settlement %+v, want the close landed", s)
		}
		got, _ := c.store.Get(claim.ID)
		if got.Assignee != c.k.ID || got.Metadata[beadmeta.StrandedSessionMetadataKey] != c.k.ID {
			t.Fatalf("claim assignee %q stamp %q, want kept and stamped", got.Assignee, got.Metadata[beadmeta.StrandedSessionMetadataKey])
		}
		if evts, _ := c.rec.List(events.Filter{Type: events.SessionStrandedClaims}); len(evts) != 1 {
			t.Fatalf("session.stranded_claims = %d, want 1", len(evts))
		}
	})
	t.Run("an unmarked pool slot with work is stamped stranded (SESS-624)", func(t *testing.T) {
		c := newCloseCase(t, livenessGone, desireSleep, "state", "asleep", "sleep_reason", "idle", "slept_at", gatherNow.Add(-time.Hour).Format(time.RFC3339))
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
		c := newCloseCase(t, livenessGone, desireSleep, "state", "asleep", "sleep_reason", "idle", "slept_at", gatherNow.Add(-time.Hour).Format(time.RFC3339), strandedEventEmittedKey, gatherNow.Add(-time.Hour).Format(time.RFC3339))
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
		c := newCloseCase(t, livenessGone, desireSleep, "state", "asleep", "sleep_reason", "killed", "slept_at", gatherNow.Add(-time.Hour).Format(time.RFC3339))
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
		c := newCloseCase(t, livenessGone, desireSleep, "state", "asleep", "sleep_reason", "killed", "slept_at", gatherNow.Add(-time.Hour).Format(time.RFC3339))
		started := c.work(t, "in_progress", routedClaim())
		before := c.row(t).Metadata[session.RuntimeLeaseEpochKey]
		s := c.run(t, gone().tmux())
		if s.Cause != causeHasWork || c.row(t).Status == "closed" || c.row(t).Metadata[session.RuntimeLeaseEpochKey] != before {
			t.Fatalf("settlement %+v, want refused has-work", s)
		}
		if got, _ := c.store.Get(started.ID); got.Assignee != c.k.ID {
			t.Fatalf("started claim assignee %q, want kept", got.Assignee)
		}
	})
	t.Run("a live pane refuses", func(t *testing.T) {
		c := newCloseCase(t, livenessGone, desireDrain, "state", "asleep")
		c.entry().DrainReason = drainOrphaned
		live := &freshObserver{Fake: runtime.NewFake(), l: runtime.Liveness{Running: true, Alive: true, ObjectID: "$1", ObjectCreated: "1"}, env: map[string]string{"GC_SESSION_ID": c.k.ID, "GC_INSTANCE_TOKEN": "tok-3"}}
		s := c.run(t, live.tmux())
		if s.Outcome != settledRefused || c.row(t).Status == "closed" {
			t.Fatalf("settlement %+v, want refused", s)
		}
	})
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
	t.Run("a corpse on a row that claims a live runtime is not A21's", func(t *testing.T) {
		c := newCloseCase(t, livenessGone, desireDrain, "state", "active")
		c.entry().DrainReason = drainOrphaned
		if s := c.run(t, corpseRead(c, false, false).tmux()); s.Cause != causeRedecided || c.row(t).Status == "closed" {
			t.Fatalf("settlement %+v, want refused redecided", s)
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
	t.Run("an unparseable marker with work takes no lease", func(t *testing.T) {
		c := newCloseCase(t, livenessGone, desireSleep, "state", "asleep", "sleep_reason", "idle", "slept_at", gatherNow.Add(-time.Hour).Format(time.RFC3339), strandedEventEmittedKey, "soon")
		c.work(t, "in_progress", routedClaim())
		before := c.row(t).Metadata[session.RuntimeLeaseEpochKey]
		if s := c.run(t, gone().tmux()); s.Cause != causeHasWork || c.row(t).Metadata[session.RuntimeLeaseEpochKey] != before {
			t.Fatalf("settlement %+v epoch %q (was %q), want refused has-work with no lease", s, c.row(t).Metadata[session.RuntimeLeaseEpochKey], before)
		}
	})
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
	if got := p.readWork(row, intentClose); !got.Free {
		t.Fatalf("close's L5 = %+v, want free: a prior alias is not the dead seat's", got)
	}
	if got := p.readWork(row, intentDrainBegin); got.Free {
		t.Fatalf("a drain's L5 = %+v, want work: the live seat may hold a prior alias's claim", got)
	}
}
