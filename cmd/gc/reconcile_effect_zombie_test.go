package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// A12 and the zombie effect (CONTRACT v5 S5; plan C5c2) on the dead kit:
// gc-1 (generation 3, token tok), not wanted awake, over s-gc-1.

// zombieKit is deadKit running the zombie effect.
func zombieKit(t *testing.T, zombie bool, id, token, pane string, meta ...string) (*txKit, *recycleLeaf) {
	t.Helper()
	k, leaf, _ := deadKit(t, zombie, id, token, pane, meta...)
	k.it.Kind = intentZombie
	return k, leaf
}

func crashFacts(s settlement) int {
	n := 0
	for _, e := range s.Facts.Events {
		if e.Type == events.SessionCrashed {
			n++
		}
	}
	return n
}

// Kills a classification that tears a pane down, a crash recorded for a
// corpse, another row's runtime or a quiet pane, and a pair classified
// again: whatever the effect finds it records the (row, token) pair, never
// kills or launches, and records one session.crashed only for the row's own
// zombie with crash output.
func TestZombieClassifiesTheRowsOwnDeadRuntime(t *testing.T) {
	for _, c := range []struct {
		name      string
		zombie    bool
		id, token string
		pane      string
		crashed   int
		classed   string
	}{
		{"own zombie, crashed", true, "gc-1", "tok", "panic: boom", 1, "gc-1/tok"},
		{"own zombie, quiet pane", true, "gc-1", "tok", "", 0, "gc-1/tok"},
		{"own corpse, crashed: a zombie's only, as legacy", false, "gc-1", "tok", "panic: boom", 0, "gc-1/tok"},
		{"another row's zombie", true, "gc-9", "theirs", "panic: boom", 0, "gc-9/theirs"},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, _ := zombieKit(t, c.zombie, c.id, c.token, c.pane)
			s := k.runKind()
			if len(k.sp.kills) != 0 || len(k.starts()) != 0 || crashFacts(s) != c.crashed {
				t.Fatalf("settlement %+v: kills %d starts %d crashes %d, want none, none, %d", s, len(k.sp.kills), len(k.starts()), crashFacts(s), c.crashed)
			}
			if s.Facts.Classified == nil || s.Facts.Classified.Key != k.it.Key || s.Facts.Classified.Token != c.classed {
				t.Fatalf("classified %+v, want %s", s.Facts.Classified, c.classed)
			}
			if k.meta("sleep_reason") != "" {
				t.Fatalf("sleep_reason %q, want nothing written", k.meta("sleep_reason"))
			}
		})
	}
	t.Run("a runtime no longer dead is recorded, nothing else", func(t *testing.T) {
		k, _ := zombieKit(t, true, "gc-1", "tok", "panic: boom")
		k.mutate(func(r *simRuntime) { r.zombie = false })
		if s := k.runKind(); s.Facts.Classified == nil || crashFacts(s) != 0 || len(k.sp.kills) != 0 {
			t.Fatalf("settlement %+v kills %d, want recorded with no crash", s, len(k.sp.kills))
		}
	})
	t.Run("a runtime gone is recorded", func(t *testing.T) {
		k, _ := zombieKit(t, true, "gc-1", "tok", "panic: boom")
		k.mutate(nil)
		if s := k.runKind(); s.Facts.Classified == nil || crashFacts(s) != 0 {
			t.Fatalf("settlement %+v, want recorded with no crash", s)
		}
	})
}

// Kills a screen classified on an unwanted row unlike the start's
// (SESS-057, SESS-088, SESS-530): a terminal error parks it, once, a row
// with only a stale provider_terminal_error mark too, and a zombie's records
// its crash as legacy's zombie capture does; a
// rate-limit screen of a crash candidate quarantines it with no crash; a
// rate-limit screen of a zombie that is no candidate records no crash.
func TestZombieDeadPaneScreens(t *testing.T) {
	recent := gatherNow.Add(-5 * time.Second).Format(time.RFC3339)
	t.Run("terminal", func(t *testing.T) {
		k, _ := zombieKit(t, true, "gc-1", "tok", "Error: model_not_found")
		if s := k.runKind(); s.Outcome != settledLanded || k.meta("sleep_reason") != string(session.SleepReasonProviderTerminalError) || crashFacts(s) != 1 || s.Facts.Classified == nil {
			t.Fatalf("settlement %+v reason %q, want parked, recorded, and one crash (legacy's zombie capture)", s, k.meta("sleep_reason"))
		}
	})
	t.Run("a corpse's terminal error parks with no crash", func(t *testing.T) {
		k, _ := zombieKit(t, false, "gc-1", "tok", "Error: model_not_found")
		if s := k.runKind(); k.meta("sleep_reason") != string(session.SleepReasonProviderTerminalError) || crashFacts(s) != 0 {
			t.Fatalf("settlement %+v reason %q, want parked with no crash", s, k.meta("sleep_reason"))
		}
	})
	t.Run("already parked", func(t *testing.T) {
		k, _ := zombieKit(t, true, "gc-1", "tok", "Error: model_not_found", "provider_terminal_error", "model_not_found", "sleep_reason", string(session.SleepReasonProviderTerminalError))
		if s := k.runKind(); s.Outcome != settledNoop || s.Facts.Classified == nil {
			t.Fatalf("settlement %+v, want a recorded no-op", s)
		}
	})
	t.Run("a stale mark only is parked", func(t *testing.T) {
		k, _ := zombieKit(t, true, "gc-1", "tok", "Error: model_not_found", "provider_terminal_error", "quota_exceeded")
		if s := k.runKind(); s.Outcome != settledLanded || k.meta("sleep_reason") != string(session.SleepReasonProviderTerminalError) {
			t.Fatalf("settlement %+v reason %q, want parked", s, k.meta("sleep_reason"))
		}
	})
	t.Run("rate limit", func(t *testing.T) {
		k, _ := zombieKit(t, true, "gc-1", "tok", "Usage limit reached", "last_woke_at", recent)
		if s := k.runKind(); k.meta("sleep_reason") != string(session.SleepReasonRateLimit) || k.meta("quarantined_until") == "" || crashFacts(s) != 0 {
			t.Fatalf("settlement %+v reason %q, want quarantined, no crash", s, k.meta("sleep_reason"))
		}
	})
	t.Run("a terminal error beside a rate-limit screen parks, with the crash", func(t *testing.T) { // terminal wins, as legacy
		k, _ := zombieKit(t, true, "gc-1", "tok", "Usage limit reached\nError: model_not_found", "last_woke_at", recent)
		s := k.runKind()
		if k.meta("sleep_reason") != string(session.SleepReasonProviderTerminalError) || crashFacts(s) != 1 {
			t.Fatalf("settlement %+v reason %q crashes %d, want parked with one crash", s, k.meta("sleep_reason"), crashFacts(s))
		}
	})
	t.Run("rate limit of no crash candidate", func(t *testing.T) {
		k, _ := zombieKit(t, true, "gc-1", "tok", "Usage limit reached")
		if s := k.runKind(); k.meta("sleep_reason") != "" || crashFacts(s) != 0 {
			t.Fatalf("settlement %+v reason %q, want nothing written and no crash", s, k.meta("sleep_reason"))
		}
	})
}

// Kills a crash recorded without legacy's crash metric: the step that
// records session.crashed hands its Call the crash, and ends after it.
func TestZombieCrashCarriesItsMetric(t *testing.T) {
	k, _ := zombieKit(t, true, "gc-1", "tok", "panic: boom")
	row := k.p.World.Census.Rows[k.it.Key].Info
	rt := &txRuntime{Class: rtZombie, Same: true, Identity: runtimeIdentity{Known: true, SessionID: "gc-1", Epoch: "3", Token: "tok"}}
	step := zombieStep(txView{It: k.it, World: k.p.World, Row: row, RT: rt, Now: gatherNow}, deadPeek{read: true, content: "panic: boom"}, nil)
	if c, ok := step.Pass.(zombieCrash); !ok || !c.found() || !step.Last || step.Done || len(step.Facts.Events) != 1 {
		t.Fatalf("step %+v, want the crash event and its metric's Call, ending after it", step)
	}
}

// Kills a classification over a failed peek or a read that does not pin
// one object: each refuses and records nothing, so A12 proposes it again.
func TestZombieRefusals(t *testing.T) {
	k, leaf := zombieKit(t, true, "gc-1", "tok", "panic: boom")
	leaf.peekErr = errors.New("peek failed")
	if s := k.runKind(); s.Cause != causePeekError || s.Facts.Classified != nil || crashFacts(s) != 0 {
		t.Fatalf("peek error: settlement %+v, want refused peek-error, unrecorded", s)
	}
	k, leaf = zombieKit(t, true, "gc-1", "tok", "panic: boom")
	n := 0
	leaf.during = func() {
		if n++; n == 2 { // between the bracketing reads
			k.runtimeAs("gc-1", "tok", func(r *simRuntime) { r.zombie = true })
		}
	}
	if s := k.runKind(); s.Cause != causeLivenessUnknown || s.Facts.Classified != nil || crashFacts(s) != 0 {
		t.Fatalf("object replaced: settlement %+v, want refused liveness-unknown, unrecorded", s)
	}
}

// Kills A12 firing on a wanted row (the start's recycle), on a row A21
// closes (C5c2 ruling 1: legacy closes it unclassified), on a live runtime,
// or again for a pair it classified: it proposes for a dead row not wanted
// awake that A21 does not close until the planner records that row's
// runtime token, and again for a new token.
func TestArmZombie(t *testing.T) {
	k, _ := zombieKit(t, true, "gc-1", "tok", "")
	base := k.p.World.Census.Rows[k.it.Key]
	id := runtimeIdentity{Known: true, SessionID: "gc-1", Epoch: "3", Token: "tok"}
	named := func(i *session.Info) { i.ConfiguredNamedSession, i.ConfiguredNamedIdentity = true, "worker" }
	parked := func(i *session.Info) { i.SleepReason = string(session.SleepReasonProviderTerminalError) }
	held := func(i *session.Info) {
		i.PoolManaged, i.QuarantinedUntil = true, gatherNow.Add(time.Hour).Format(time.RFC3339)
	}
	young := func(i *session.Info) {
		i.MetadataState, i.LastWokeAt = "awake", gatherNow.Add(-time.Minute).Format(time.RFC3339)
	}
	for _, c := range []struct {
		name       string
		liveness   rowLiveness
		desired    desire
		info       func(*session.Info)
		classified map[rowKey]string
		want       bool
	}{
		{"dead, drained: A21 closes it unclassified", livenessDead, desireDrain, nil, nil, false},
		{"dead, drained inside its grace: A21's once it ends", livenessDead, desireDrain, young, nil, false},
		{"dead, drained, named: not A21's", livenessDead, desireDrain, named, nil, true},
		{"dead, drained, parked: not A21's", livenessDead, desireDrain, parked, nil, true},
		{"dead, drained, a held pool slot: not A21's while held", livenessDead, desireDrain, held, nil, true},
		{"dead, sleeping", livenessDead, desireSleep, nil, nil, true},
		{"dead, wanted awake", livenessDead, desireWake, nil, nil, false},
		{"alive", livenessAlive, desireSleep, nil, nil, false},
		{"gone", livenessGone, desireSleep, nil, nil, false},
		{"classified", livenessDead, desireSleep, nil, map[rowKey]string{k.it.Key: "gc-1/tok"}, false},
		{"classified, another token", livenessDead, desireSleep, nil, map[rowKey]string{k.it.Key: "gc-1/old"}, true},
	} {
		w := *k.p.World
		w.Observed = map[rowKey]rowObservation{k.it.Key: {Liveness: c.liveness, Identity: id}}
		w.Classified = c.classified
		row := base
		if c.info != nil {
			c.info(&row.Info)
		}
		r := &rowFacts{w: &w, k: k.it.Key, row: row, found: true, entry: &selectionEntry{Key: k.it.Key, Liveness: c.liveness, Desired: c.desired, DrainReason: drainOrphaned, Template: "worker"}}
		it, ok := armZombie(r)
		if ok != c.want || ok && (it.Kind != intentZombie || it.Reason != decideZombie || r.rests&restRuntime == 0) {
			t.Fatalf("%s: armZombie = (%+v, %v), want %v", c.name, it, ok, c.want)
		}
	}
	// A Drain row with no drain reason is not A21's orphan close (its guard).
	w := *k.p.World
	w.Observed = map[rowKey]rowObservation{k.it.Key: {Liveness: livenessDead, Identity: id}}
	r := &rowFacts{w: &w, k: k.it.Key, row: base, found: true, entry: &selectionEntry{Key: k.it.Key, Liveness: livenessDead, Desired: desireDrain, Template: "worker"}}
	if _, ok := armZombie(r); !ok {
		t.Fatal("a Drain row with no drain reason: want A12, not A21's close")
	}
}

// Kills a classification never cleared when the runtime recovers (SESS-090:
// legacy re-fires on its next death): the planner forgets a row whose
// runtime reads alive or gone, or that left the census, and keeps a dead
// one's.
func TestPlannerForgetsARecoveredClassification(t *testing.T) {
	k, _ := zombieKit(t, true, "gc-1", "tok", "")
	key := k.it.Key
	for _, c := range []struct {
		liveness rowLiveness
		kept     bool
	}{{livenessDead, true}, {livenessAlive, false}, {livenessGone, false}} {
		p := &planner{classified: map[rowKey]string{key: "gc-1/tok"}}
		got := p.classifiedOf(k.p.World.Census, map[rowKey]rowObservation{key: {Liveness: c.liveness}})
		if _, ok := got[key]; ok != c.kept {
			t.Fatalf("%v: classified %v, want kept %v", c.liveness, got, c.kept)
		}
	}
	p := &planner{classified: map[rowKey]string{{ID: "gone-row"}: "x/y"}}
	if got := p.classifiedOf(k.p.World.Census, nil); len(got) != 0 {
		t.Fatalf("classified %v, want a row out of the census forgotten", got)
	}
}

// crashMetrics records the crash metric's calls while t runs.
func crashMetrics(t *testing.T) *[]string {
	t.Helper()
	var got []string
	saved := recordAgentCrash
	recordAgentCrash = func(_ context.Context, agent, output string) { got = append(got, agent+": "+output) }
	t.Cleanup(func() { recordAgentCrash = saved })
	return &got
}

// Kills the crash metric dropped from A12's crash or the recycle's (B12,
// B28): each records it once with the crash event, and a quiet pane none.
func TestCrashMetric(t *testing.T) {
	got := crashMetrics(t)
	k, _ := zombieKit(t, true, "gc-1", "tok", "panic: boom")
	if k.runKind(); len(*got) != 1 {
		t.Fatalf("A12: crash metrics %v, want one", *got)
	}
	*got = nil
	k, _ = zombieKit(t, true, "gc-1", "tok", "")
	if k.runKind(); len(*got) != 0 {
		t.Fatalf("A12, a quiet pane: crash metrics %v, want none", *got)
	}
	*got = nil
	k, _, rec := deadKit(t, true, "gc-1", "tok", "panic: boom")
	if k.runKind(); len(*got) != 1 || crashes(rec) != 1 {
		t.Fatalf("recycle: crash metrics %v crashes %d, want one of each", *got, crashes(rec))
	}
}

// Kills A4 or A14 reading a classified dead runtime as gone (the C5c2
// delta review): freshGone stays strict; only A6's asleep heals take a
// classified one (freshGoneOrClassified).
func TestClassifiedDeadIsNotGone(t *testing.T) {
	k, _ := zombieKit(t, true, "gc-1", "tok", "")
	id := runtimeIdentity{Known: true, SessionID: "gc-1", Epoch: "3", Token: "tok"}
	w := *k.p.World
	w.Observed = map[rowKey]rowObservation{k.it.Key: {Liveness: livenessDead, Identity: id}}
	w.Classified = map[rowKey]string{k.it.Key: deadToken(id)}
	r := &rowFacts{w: &w, k: k.it.Key, row: k.p.World.Census.Rows[k.it.Key], found: true, entry: &selectionEntry{Key: k.it.Key, Liveness: livenessDead, Desired: desireSleep}}
	if r.freshGone() || !r.freshGoneOrClassified() {
		t.Fatalf("freshGone %v freshGoneOrClassified %v, want false and true", r.freshGone(), r.freshGoneOrClassified())
	}
	r.fresh = &txRuntime{Class: rtZombie, Same: true, Identity: id}
	if r.freshGone() || !r.freshGoneOrClassified() {
		t.Fatalf("fresh read: freshGone %v freshGoneOrClassified %v, want false and true", r.freshGone(), r.freshGoneOrClassified())
	}
}

// Kills A4 finalizing a signaled stop over a classified zombie (E01, the
// C5c2 delta review): it decides the stop verb's stop, never its finalize,
// which only a runtime that reads gone takes.
func TestStopOnAClassifiedZombieIsNoFinalize(t *testing.T) {
	c := newHealCase(t, livenessDead, desireSleep, "state", string(session.StateDraining), "state_reason", session.DrainAckStopPendingReason)
	id := runtimeIdentity{Known: true, SessionID: c.k.ID, Epoch: "3", Token: "tok-3"}
	c.w.Observed = map[rowKey]rowObservation{c.k: {Liveness: livenessDead, Identity: id}}
	c.w.Classified = map[rowKey]string{c.k: deadToken(id)}
	if it := c.decide(); it.Kind != intentStop || it.Finalize {
		t.Fatalf("decideRow = (%q, %q, finalize %v), want the stop with no finalize", it.Kind, it.Reason, it.Finalize)
	}
}
