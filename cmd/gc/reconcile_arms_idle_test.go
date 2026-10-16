package main

import (
	"cmp"
	"context"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// idleCase is a live desired pool row of a worker whose idle_timeout is
// 5m, its runtime read current at censusNow, last active idle ago (unknown
// when negative), its pane content read content.
func idleCase(t *testing.T, cfg *config.City, idle time.Duration, content contentState, meta ...string) *healCase {
	t.Helper()
	c := newHealCase(t, livenessAlive, desireKeep, meta...)
	c.a.Snapshot.Entries[c.k].InDesired = true
	c.w.Now, c.w.ObsMaxAge = censusNow, observeMaxAge
	c.w.Env = &reconcileEnv{Cfg: cfg}
	info := c.w.Census.Rows[c.k].Info
	c.w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: TemplateParams{TemplateName: "worker"}}}}
	attrs := InventoryAttrs{Identity: runtimeIdentity{Known: true, SessionID: c.k.ID, Token: "tok-3"}, Content: content}
	if idle >= 0 {
		attrs.ActivityKnown, attrs.LastActivity = true, censusNow.Add(-idle)
	}
	c.w.Obs = newObserveCache().publish(censusNow, map[string]InventoryAttrs{"s-heal": attrs}, completeBackend("tmux", "s-heal"))
	return c
}

// idleView sets the pass's A15 view: the defer streaks and the content
// clock's fires.
func (c *healCase) idleView(defers map[rowKey]int, fired map[rowKey]bool) {
	c.w.Idle = &idlePass{defers: defers, fired: fired}
}

func idleCity(mut ...func(*config.Agent)) *config.City {
	cfg := workerCity(3)
	cfg.Agents[0].IdleTimeout = "5m"
	for _, m := range mut {
		m(&cfg.Agents[0])
	}
	return cfg
}

func (c *healCase) idleArm() (intent, bool) {
	row := c.w.Census.Rows[c.k]
	return armIdleTimeout(&rowFacts{w: c.w, k: c.k, entry: c.a.Snapshot.Entries[c.k], row: row})
}

// Kills a ladder out of legacy's order (v5 A15, v5.9s, SESS-574/575/576): a
// live desired row idle past its idle_timeout, by its activity or its
// content clock, is stopped; one with no timeout, recent or unknown
// activity, a lifecycle blocker, a pending interaction (but under a wait
// hold), a min-floor seat, assigned work within its defer limit, an attach
// or an unknown attach this pass, or a runtime not its own is not; work
// past the limit is the forced stop, ahead of the min-floor exemption; a
// zero limit is the default.
func TestArmIdleTimeout(t *testing.T) {
	work := func(c *healCase) { c.a.Snapshot.Entries[c.k].AssignedWork = &assignedWorkView{BeadID: "gw-1"} }
	pending := func(c *healCase) { c.a.Snapshot.Entries[c.k].WakeReasons = []WakeReason{WakePending} }
	for _, tc := range []struct {
		name  string
		cfg   *config.City
		idle  time.Duration
		meta  []string
		setup func(*healCase)
		want  string
	}{
		{name: "idle past its timeout", cfg: idleCity(), idle: 10 * time.Minute, want: decideIdleTimeout},
		{name: "active within it", cfg: idleCity(), idle: 4 * time.Minute},
		{name: "activity unknown", cfg: idleCity(), idle: -1},
		{name: "the content clock fired", cfg: idleCity(), idle: -1, setup: func(c *healCase) { c.idleView(nil, map[rowKey]bool{c.k: true}) }, want: decideIdleTimeout},
		{name: "no idle_timeout", cfg: workerCity(3), idle: time.Hour},
		{name: "a user hold", cfg: idleCity(), idle: time.Hour, meta: []string{"held_until", rowAt(time.Hour)}},
		{name: "a pin", cfg: idleCity(), idle: time.Hour, meta: []string{"pin_awake", "true"}},
		{name: "not desired", cfg: idleCity(), idle: time.Hour, setup: func(c *healCase) { c.a.Snapshot.Entries[c.k].InDesired = false }},
		{name: "assigned work within the limit", cfg: idleCity(), idle: time.Hour, setup: func(c *healCase) { work(c); c.idleView(map[rowKey]int{c.k: 3}, nil) }},
		{name: "assigned work past the limit", cfg: idleCity(), idle: time.Hour, setup: func(c *healCase) { work(c); c.idleView(map[rowKey]int{c.k: 4}, nil) }, want: decideIdleExhausted},
		{name: "a configured limit", cfg: idleCity(func(a *config.Agent) { a.AssignedWorkDeferLimit = intPtr(1) }), idle: time.Hour, setup: func(c *healCase) { work(c); c.idleView(map[rowKey]int{c.k: 2}, nil) }, want: decideIdleExhausted},
		{name: "a zero limit is the default, within it", cfg: idleCity(func(a *config.Agent) { a.AssignedWorkDeferLimit = intPtr(0) }), idle: time.Hour, setup: func(c *healCase) { work(c); c.idleView(map[rowKey]int{c.k: 3}, nil) }},
		{name: "a min-floor member past its limit", cfg: idleCity(func(a *config.Agent) { a.MinActiveSessions = intPtr(1) }), idle: time.Hour, meta: []string{"state", "active", "pool_managed", "true", "pool_slot", "1"}, setup: func(c *healCase) { work(c); c.idleView(map[rowKey]int{c.k: 4}, nil) }, want: decideIdleExhausted},
		{name: "a pending interaction", cfg: idleCity(), idle: time.Hour, setup: pending},
		{name: "a pending interaction under a wait hold", cfg: idleCity(), idle: time.Hour, meta: []string{"wait_hold", "true"}, setup: pending, want: decideIdleTimeout},
		{name: "attached this pass", cfg: idleCity(), idle: time.Hour, setup: func(c *healCase) {
			c.w.Observed = map[rowKey]rowObservation{c.k: {Liveness: livenessAlive, Attached: true}}
		}},
		{name: "its attach unknown this pass", cfg: idleCity(), idle: time.Hour, setup: func(c *healCase) {
			c.w.Observed = map[rowKey]rowObservation{c.k: {Liveness: livenessAlive, Uncertain: true}}
		}},
		{name: "a min-floor member", cfg: idleCity(func(a *config.Agent) { a.MinActiveSessions = intPtr(1) }), idle: time.Hour, meta: []string{"state", "active", "pool_managed", "true", "pool_slot", "1"}},
		{name: "a runtime not its own", cfg: idleCity(), idle: time.Hour, meta: []string{"instance_token", "tok-other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := idleCase(t, tc.cfg, tc.idle, contentUnread, tc.meta...)
			if tc.setup != nil {
				tc.setup(c)
			}
			it, ok := c.idleArm()
			if got := map[bool]string{true: it.Reason}[ok]; got != tc.want {
				t.Fatalf("armIdleTimeout = %q (%v), want %q", got, ok, tc.want)
			}
			if ok && (it.Kind != intentStop || it.Finish == nil) {
				t.Fatalf("intent %+v, want a stop with the idle finalize", it)
			}
		})
	}
}

// Kills an idle finalize that parks or closes the row (v5.7, SESS-576): it
// sleeps it, with legacy's sleep reason.
func TestIdleFinishes(t *testing.T) {
	for _, tc := range []struct {
		fin    func(finalizeFacts) finalizeChoice
		reason session.SleepReason
	}{{idleTimeoutFinish, session.SleepReasonIdleTimeout}, {idleExhaustedFinish, session.SleepReasonAssignedWorkExhausted}} {
		if got := tc.fin(finalizeFacts{Now: censusNow}); !got.AsSleep || got.Patch["sleep_reason"] != string(tc.reason) {
			t.Fatalf("finalize %+v, want a sleep for %q", got, tc.reason)
		}
	}
}

// Kills a defer streak legacy would not count (SESS-575, v5.9s): a row
// deferred on assigned work counts one a patrol, not one a pass, a new
// anchor bead (its currently_processing_bead_id) restarts at one, and a
// pass without the defer, a pending interaction's included, forgets it.
func TestIdleMemoryStreak(t *testing.T) {
	const patrol = 15 * time.Second
	var m idleMemory
	for i, step := range []struct {
		at      time.Duration
		bead    string // the row's current bead; "" for no assigned work
		work    string // the pass's assigned work, when it differs
		pending bool
		want    int
	}{
		{at: 0, bead: "gw-1", want: 1},
		{at: time.Second, bead: "gw-1", want: 1}, // a pass inside the patrol
		{at: patrol, bead: "gw-1", want: 2},
		{at: 2 * patrol, bead: "gw-2", work: "gw-1", want: 1}, // keyed on the current bead, not the pass's work
		{at: 3 * patrol, bead: "gw-2", pending: true},
		{at: 4 * patrol, bead: "gw-2", want: 1},
		{at: 5 * patrol, want: 0},
	} {
		c := idleCase(t, idleCity(), time.Hour, contentUnread, session.CurrentBeadIDKey, step.bead)
		c.w.Now, c.w.ObsMaxAge = censusNow.Add(step.at), time.Hour
		e := c.a.Snapshot.Entries[c.k]
		if step.bead != "" {
			e.AssignedWork = &assignedWorkView{BeadID: cmp.Or(step.work, step.bead)}
		}
		if step.pending {
			e.WakeReasons = []WakeReason{WakePending}
		}
		m.update(c.w, c.a, patrol, io.Discard)
		if got := c.w.Idle.defers[c.k]; got != step.want {
			t.Fatalf("pass %d: streak %d, want %d", i, got, step.want)
		}
	}
}

// Kills a forced stop over work right after a prompt is answered (the C7a-1
// review's probe): a pending interaction holds the ladder and resets the
// streak every pass, so the pass after it clears counts one.
func TestIdlePendingResetsTheStreak(t *testing.T) {
	var m idleMemory
	c := idleCase(t, idleCity(), time.Hour, contentUnread, session.CurrentBeadIDKey, "gw-1")
	e := c.a.Snapshot.Entries[c.k]
	e.AssignedWork, e.WakeReasons = &assignedWorkView{BeadID: "gw-1"}, []WakeReason{WakePending}
	for range 5 {
		m.update(c.w, c.a, 0, io.Discard)
		if _, ok := c.idleArm(); ok || c.w.Idle.defers[c.k] != 0 {
			t.Fatalf("pending: streak %d, decided %v; want held, none counted", c.w.Idle.defers[c.k], ok)
		}
	}
	e.WakeReasons = nil
	m.update(c.w, c.a, 0, io.Discard)
	if it, ok := c.idleArm(); ok || c.w.Idle.defers[c.k] != 1 {
		t.Fatalf("answered: streak %d, decided %q; want one, held on the work", c.w.Idle.defers[c.k], it.Reason)
	}
}

// Kills a content anchor that counts unknown as idle, fires early, or
// outlives its runtime (ga-07mi8, v5.9s): an idle pane anchors and fires
// strictly past the timeout (and re-anchors); a busy, indeterminate,
// unread or erring read clears it; a new runtime token or a relaunched
// pane (a new observed incarnation) starts a fresh clock; a gone row's anchor is pruned; a row off the content clock is
// neither read nor anchored.
func TestIdleMemoryContentAnchors(t *testing.T) {
	claude := []string{"provider", "claude"}
	pass := func(m *idleMemory, at time.Duration, content contentState, meta ...string) *healCase {
		c := idleCase(t, idleCity(), -1, content, append(slices.Clone(claude), meta...)...)
		c.w.Now, c.w.ObsMaxAge = censusNow.Add(at), time.Hour
		want, _ := m.update(c.w, c.a, 0, io.Discard)
		if !want["s-heal"] {
			t.Fatalf("content wanted %v, want the row's pane", want)
		}
		return c
	}
	var m idleMemory
	for i, step := range []struct {
		at      time.Duration
		content contentState
		fires   bool
	}{
		{0, contentIdle, false},
		{5 * time.Minute, contentIdle, false}, // at the timeout, not past it
		{5*time.Minute + time.Second, contentIdle, true},
		{6 * time.Minute, contentIdle, false}, // re-anchored
		{12 * time.Minute, contentBusy, false},
		{13 * time.Minute, contentIdle, false},
		{19 * time.Minute, contentIndeterminate, false},
		{20 * time.Minute, contentIdle, false},
		{26 * time.Minute, contentError, false},
		{27 * time.Minute, contentIdle, false},
		{33 * time.Minute, contentUnread, false},
		{34 * time.Minute, contentIdle, false},
		{39*time.Minute + time.Second, contentIdle, true},
	} {
		if c := pass(&m, step.at, step.content); c.w.Idle.fired[c.k] != step.fires {
			t.Fatalf("pass %d (%v, content %d): fired %v, want %v", i, step.at, step.content, c.w.Idle.fired[c.k], step.fires)
		}
	}
	var r idleMemory
	pass(&r, 0, contentIdle)
	if c := pass(&r, 6*time.Minute, contentIdle, "instance_token", "tok-new"); c.w.Idle.fired[c.k] || len(r.since) != 1 {
		t.Fatalf("a new runtime token: fired %v, anchors %v; want a fresh clock", c.w.Idle.fired[c.k], r.since)
	}
	// A relaunch keeps the row's token and generation; its new pane is a
	// new observed incarnation, which starts a fresh clock.
	var rl idleMemory
	relaunched := func(at time.Duration, incarnation string) *healCase {
		c := idleCase(t, idleCity(), -1, contentIdle, claude...)
		c.w.Now, c.w.ObsMaxAge = censusNow.Add(at), time.Hour
		attrs := InventoryAttrs{Identity: runtimeIdentity{Known: true, SessionID: c.k.ID, Token: "tok-3"}, Content: contentIdle, Incarnation: incarnation}
		c.w.Obs = newObserveCache().publish(c.w.Now, map[string]InventoryAttrs{"s-heal": attrs}, completeBackend("tmux", "s-heal"))
		rl.update(c.w, c.a, 0, io.Discard)
		return c
	}
	relaunched(0, "$1:100:41")
	if c := relaunched(6*time.Minute, "$1:100:57"); c.w.Idle.fired[c.k] || len(rl.since) != 1 {
		t.Fatalf("a relaunched pane: fired %v, anchors %v; want a fresh clock", c.w.Idle.fired[c.k], rl.since)
	}
	if c := relaunched(12*time.Minute, "$1:100:57"); !c.w.Idle.fired[c.k] {
		t.Fatal("the relaunched pane idle past its timeout did not fire")
	}
	c := idleCase(t, idleCity(), -1, contentIdle, claude...)
	c.a.Snapshot.Entries[c.k].Liveness = livenessGone
	if want, _ := r.update(c.w, c.a, 0, io.Discard); len(r.since) != 0 || len(want) != 0 {
		t.Fatalf("a gone row: anchors %v, wanted %v; want none", r.since, want)
	}
	var o idleMemory
	c = idleCase(t, idleCity(), -1, contentIdle)
	if want, _ := o.update(c.w, c.a, 0, io.Discard); len(o.since) != 0 || len(want) != 0 {
		t.Fatalf("a non-claude row: anchors %v, wanted %v; want none", o.since, want)
	}
}

// Kills a deferral trace legacy emits and v2 drops, or one emitted every
// pass: a held row's idle-timeout deferral is traced at its site with
// legacy's codes, again only a patrol later or on another reason.
func TestIdleMemoryTracesDeferrals(t *testing.T) {
	const patrol = 15 * time.Second
	var m idleMemory
	for i, step := range []struct {
		at   time.Duration
		meta []string
		want string
	}{
		{0, []string{"held_until", rowAt(time.Hour)}, "user_hold/deferred_user_hold"},
		{time.Second, []string{"held_until", rowAt(time.Hour)}, ""},
		{2 * time.Second, []string{"pin_awake", "true"}, "pinned/deferred_pinned"},
		{2*time.Second + patrol, []string{"pin_awake", "true"}, "pinned/deferred_pinned"},
	} {
		c := idleCase(t, idleCity(), time.Hour, contentUnread, step.meta...)
		c.w.Now, c.w.ObsMaxAge = censusNow.Add(step.at), time.Hour
		_, traces := m.update(c.w, c.a, patrol, io.Discard)
		var got string
		for _, tr := range traces {
			if tr.Site != TraceSiteReconcilerIdleTimeout || tr.Template != "worker" || tr.SessionName != "s-heal" {
				t.Fatalf("pass %d: trace %+v, want the idle-timeout site for worker/s-heal", i, tr)
			}
			got = tr.Reason + "/" + tr.Outcome
		}
		if got != step.want {
			t.Fatalf("pass %d: traced %q, want %q", i, got, step.want)
		}
	}
}

// Kills a lane that never reads the content clock's panes (the planner's
// WantContent wiring): a pass on a Claude row asks the observation cache
// for its pane.
func TestPassWantsTheContentClocksPanes(t *testing.T) {
	c := idleCase(t, idleCity(), -1, contentUnread, "provider", "claude")
	p := newPlanner(newFakePlannerClock(plannerT0), func() time.Duration { return time.Minute }, nil, newInflightMap(), nil, io.Discard)
	cache := newObserveCache()
	p.observations = func() *ObservationCache { return cache.ObservationCache }
	p.idleWants(c.w, c.a)
	if !cache.contentWanted()["s-heal"] {
		t.Fatalf("content wanted %v, want the Claude row's pane", cache.contentWanted())
	}
}

// Kills a lifecycle setting resolved other than legacy's trackers resolve
// it: a named session's own agent's (but never an always one's), a pool
// template's to its every row but an always named session's on it, a
// singleton's to its one runtime name.
func TestLifecycleSetting(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(3), IdleTimeout: "1m"},
			{Name: "solo", StartCommand: "true", MaxActiveSessions: intPtr(1), IdleTimeout: "2m"},
			{Name: "chat", StartCommand: "true", IdleTimeout: "3m"},
			{Name: "boss", StartCommand: "true", IdleTimeout: "4m"},
		},
		NamedSessions: []config.NamedSession{{Template: "chat", Mode: "on_demand"}, {Template: "boss", Mode: "always"}, {Name: "boss-pool", Template: "worker", Mode: "always"}, {Name: "helper-pool", Template: "worker", Mode: "on_demand"}},
	}
	for _, tc := range []struct {
		template, name string
		want           time.Duration
	}{
		{"worker", "worker-7", time.Minute},
		{"solo", "solo", 2 * time.Minute},
		{"solo", "solo-other", 0},
		{"chat", "chat", 3 * time.Minute},
		{"boss", "boss", 0},
		{"nobody", "nobody", 0},
		{"worker", "boss-pool", 0}, // an always named session on a pool template
		{"worker", "helper-pool", time.Minute},
	} {
		if got := idleTimeoutFor(cfg, tc.template, tc.name); got != tc.want {
			t.Errorf("idleTimeoutFor(%q, %q) = %v, want %v", tc.template, tc.name, got, tc.want)
		}
	}
}

// idleOn decides an idle row on admittedBeginOn's leaf by its activity
// clock (the lane's last activity 10m old, the leaf's too), its defer
// streak past any limit, its identity metadata settled first (the probe's
// A7 patch seeded), so A15 decides it.
func idleOn(t *testing.T, exhausted bool, extra ...string) (*effectPass, intent, beads.Store, *fenceLeaf) {
	t.Helper()
	entry := func(e *selectionEntry) {
		e.InDesired, e.Desired = true, desireKeep
		if exhausted {
			e.AssignedWork = &assignedWorkView{BeadID: "gw-1"}
		}
	}
	world := func(w *World) {
		w.Env.Cfg.Agents[0].IdleTimeout = "5m"
		w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{}}
		w.Idle = &idlePass{defers: map[rowKey]int{}}
		attrs := map[string]InventoryAttrs{}
		var names []string
		for _, row := range w.Census.Rows {
			w.Templates.entries[templateMemoKeyOf(row.Info)] = templateResolution{TP: TemplateParams{TemplateName: "worker"}}
			w.Idle.defers[row.Key] = 9
			info := row.Info
			if info.SessionName == "" {
				continue
			}
			ident := runtimeIdentity{Known: true, SessionID: info.ID, Token: info.InstanceToken}
			attrs[info.SessionName], names = InventoryAttrs{Identity: ident, ActivityKnown: true, LastActivity: w.Now.Add(-10 * time.Minute)}, append(names, info.SessionName)
		}
		w.Obs = newObserveCache().publish(w.Now, attrs, completeBackend("tmux", names...))
	}
	probe, it, _, _ := admittedBeginOn(t, extra, entry, world)
	meta := slices.Clone(extra)
	if it.Kind == intentRowMetadata {
		row := probe.World.Census.Rows[it.Key]
		id, blocked, _, _ := probe.World.rowIdentityOf(it.Key, row.Info, row.Refresh)
		for k, v := range identityPatch(id, row.Info, blocked, nil, nil, "") {
			meta = append(meta, k, v)
		}
	}
	p, it, store, leaf := admittedBeginOn(t, meta, entry, world)
	leaf.SetActivity("s-gc-1", time.Now().Add(-10*time.Minute)) // the fresh read the stop re-proves on
	return p, it, store, leaf
}

// claimWork claims a work bead for the row id: in progress, assigned.
func claimWork(t *testing.T, store beads.Store, id string) {
	t.Helper()
	b, err := store.Create(beads.Bead{Title: "w", Type: "task", Assignee: id})
	if err == nil {
		err = store.Update(b.ID, beads.UpdateOpts{Status: stringPtr("in_progress")})
	}
	if err != nil {
		t.Fatal(err)
	}
}

// Kills an idle kill on a stale read, over a human, a prompt or a claim
// since the pass, or one that loses legacy's side effects (v5.9s,
// SESS-574/575/576): the idle stop refuses on a fresh activity (the agent
// started a turn after the lane's read), a fresh attach, a prompt (but
// under a wait hold, as the ladder exempts it) or an in-progress claim; an open bead it holds unclaimed does not hold it
// (legacy's late claim); the forced stop stops over the claim; a landed
// idle stop clears the pane's scrollback, emits session.idle_killed and
// sleeps the row with its reason; every outcome settles on the idle key.
func TestIdleStopThroughTheEffectPass(t *testing.T) {
	for _, tc := range []struct {
		name      string
		exhausted bool
		meta      []string
		set       func(*testing.T, *fenceLeaf, beads.Store, string)
		cause     string // a refusal's; "" lands
	}{
		{name: "idle"},
		{name: "active since the lane's read", set: func(_ *testing.T, l *fenceLeaf, _ beads.Store, _ string) { l.SetActivity("s-gc-1", time.Now()) }, cause: causeIdleUnproven},
		{name: "its activity unreadable", set: func(_ *testing.T, l *fenceLeaf, _ beads.Store, _ string) { l.SetActivity("s-gc-1", time.Time{}) }, cause: causeIdleUnproven},
		{name: "attached since the pass", set: func(_ *testing.T, l *fenceLeaf, _ beads.Store, _ string) { l.SetAttached("s-gc-1", true) }, cause: fenceAttached},
		{name: "a pending interaction", set: func(_ *testing.T, l *fenceLeaf, _ beads.Store, _ string) {
			l.SetPendingInteraction("s-gc-1", &runtime.PendingInteraction{RequestID: "r", Kind: "approval"})
		}, cause: fencePending},
		{name: "a pending interaction under a wait hold", meta: []string{"wait_hold", "true"}, set: func(_ *testing.T, l *fenceLeaf, _ beads.Store, _ string) {
			l.SetPendingInteraction("s-gc-1", &runtime.PendingInteraction{RequestID: "r", Kind: "approval"})
		}},
		{name: "a claim since the pass", set: func(t *testing.T, _ *fenceLeaf, s beads.Store, id string) { claimWork(t, s, id) }, cause: fenceHasWork},
		{name: "an open bead unclaimed", set: func(t *testing.T, _ *fenceLeaf, s beads.Store, id string) {
			if _, err := s.Create(beads.Bead{Title: "blocked", Type: "task", Status: "open", Assignee: id}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "exhausted, over a claim", exhausted: true, set: func(t *testing.T, _ *fenceLeaf, s beads.Store, id string) { claimWork(t, s, id) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, it, store, leaf := idleOn(t, tc.exhausted, tc.meta...)
			want := map[bool]string{false: decideIdleTimeout, true: decideIdleExhausted}[tc.exhausted]
			if it.Kind != intentStop || it.Reason != want {
				t.Fatalf("decided (%q, %q) %v, want the stop for %q", it.Kind, it.Reason, it.Patch, want)
			}
			if tc.set != nil {
				tc.set(t, leaf, store, it.Key.ID)
			}
			s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
			if key := settledBackoffKey(it.Kind, it.Reason, it.Key); key != idleBackoffKey(it.Key) {
				t.Fatalf("settles on %q, want the idle key", key)
			}
			got, _ := store.Get(it.Key.ID)
			if tc.cause != "" {
				if s.Outcome != settledRefused || s.Cause != tc.cause || stops(leaf) != 0 || got.Metadata["state"] == "asleep" {
					t.Fatalf("settled %+v, stops %d, state %q; want refused %q, nothing stopped", s, stops(leaf), got.Metadata["state"], tc.cause)
				}
				return
			}
			reason := map[bool]session.SleepReason{false: session.SleepReasonIdleTimeout, true: session.SleepReasonAssignedWorkExhausted}[tc.exhausted]
			switch {
			case s.Outcome != settledLanded || stops(leaf) != 1:
				t.Fatalf("settled %+v, stops %d; want one landed stop", s, stops(leaf))
			case leaf.CountCalls("ClearScrollback", "s-gc-1") != 1:
				t.Fatal("the idle stop left the scrollback")
			case got.Metadata["sleep_reason"] != string(reason) || s.Facts.IdleKilled == "" || len(s.Facts.Events) != 1 || s.Facts.Events[0].Type != events.SessionIdleKilled:
				t.Fatalf("row %v, facts %+v; want asleep for %q and the idle kill", got.Metadata, s.Facts, reason)
			}
		})
	}
}

// Kills an idle stop that leaves a requested drain's residue for A19 (the
// fresh row's stop keys, Census.reread): the idle finalize voids it.
func TestIdleStopVoidsARequestedDrain(t *testing.T) {
	p, it, store, _ := idleOn(t, false, intentAt(drainIdle, "3")...)
	if it.Reason != decideIdleTimeout {
		t.Fatalf("decided (%q, %q), want the idle stop", it.Kind, it.Reason)
	}
	s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
	if got, _ := store.Get(it.Key.ID); s.Outcome != settledLanded || got.Metadata[session.DrainIntentReasonKey] != "" {
		t.Fatalf("settled %+v, drain request %q; want landed and voided", s, got.Metadata[session.DrainIntentReasonKey])
	}
}

// Kills an idle proof that counts unknown as idle (v5.9s): a content-clock
// fire is proved only by a fresh idle snapshot; the activity clock only by
// a reportable, clean, non-zero fresh read strictly older than the
// timeout.
func TestIdleReproved(t *testing.T) {
	c := idleCase(t, idleCity(), time.Hour, contentUnread)
	read := func(ago time.Duration, err error) *activityRead {
		return &activityRead{Reportable: true, Last: censusNow.Add(-ago), Err: err}
	}
	for _, tc := range []struct {
		name  string
		fired bool
		fence txFence
		want  bool
	}{
		{"content idle", true, txFence{Content: contentIdle}, true},
		{"content busy", true, txFence{Content: contentBusy}, false},
		{"content unread", true, txFence{}, false},
		{"content fired, the activity old", true, txFence{Activity: read(time.Hour, nil)}, false},
		{"activity past the timeout", false, txFence{Activity: read(5*time.Minute+time.Second, nil)}, true},
		{"activity at the timeout", false, txFence{Activity: read(5*time.Minute, nil)}, false},
		{"activity recent", false, txFence{Activity: read(time.Minute, nil)}, false},
		{"activity erring", false, txFence{Activity: read(time.Hour, errProbeExpired)}, false},
		{"activity zero", false, txFence{Activity: &activityRead{Reportable: true}}, false},
		{"activity unreportable", false, txFence{Activity: &activityRead{Last: censusNow.Add(-time.Hour)}}, false},
		{"activity unread", false, txFence{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c.idleView(nil, map[rowKey]bool{c.k: tc.fired})
			v := txView{It: intent{Key: c.k, Kind: intentStop, Reason: decideIdleTimeout}, World: c.w, Row: c.w.Census.Rows[c.k].Info, Fence: tc.fence, Now: censusNow}
			if got := idleReproved(v); got != tc.want {
				t.Fatalf("idleReproved = %v, want %v", got, tc.want)
			}
		})
	}
}

// Kills a content-clock stop that reads no fresh snapshot, or one a hung
// snapshot holds (v5.9s): needs.Content reads the routed leaf's pane,
// bounded; a leaf that cannot snapshot reads unread; the idle stop asks
// for the pane on a content-clock fire and the activity otherwise.
func TestIdleStopReadsItsProof(t *testing.T) {
	leaf := &contentLeaf{Fake: runtime.NewFake(), idle: map[string]bool{"idle": true}, err: map[string]error{"unsure": runtime.ErrIdleIndeterminate}, block: map[string]bool{"slow": true}, done: make(chan struct{})}
	t.Cleanup(func() { close(leaf.done) })
	for name, want := range map[string]contentState{"idle": contentIdle, "unsure": contentIndeterminate} {
		if got := readLegs(context.Background(), leaf, name, needs{Content: true}, gatherNow, gatherNow, nil).Content; got != want {
			t.Errorf("%s: content %d, want %d", name, got, want)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	hung := make(chan contentState, 1)
	go func() { hung <- readLegs(ctx, leaf, "slow", needs{Content: true}, gatherNow, gatherNow, nil).Content }()
	select { // the read respects its bound: it returns, the hung snapshot abandoned
	case got := <-hung:
		if got != contentUnread {
			t.Errorf("a hung snapshot: %d, want unread", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the content read outlived its bound on a hung snapshot")
	}
	if got := readLegs(context.Background(), runtime.NewFake(), "idle", needs{Content: true}, gatherNow, gatherNow, nil).Content; got != contentUnread {
		t.Errorf("no snapshot: %d, want unread", got)
	}
	c := idleCase(t, idleCity(), time.Hour, contentUnread)
	it := intent{Key: c.k, Kind: intentStop, Reason: decideIdleTimeout}
	c.idleView(nil, map[rowKey]bool{c.k: true})
	if n := stopNeeds(c.w, it); !n.Content || n.Activity {
		t.Fatalf("a content-clock fire: needs %+v, want the pane", n)
	}
	c.idleView(nil, nil)
	if n := stopNeeds(c.w, it); n.Content || !n.Activity {
		t.Fatalf("the activity clock: needs %+v, want the activity", n)
	}
}

// Kills the idle finalize applied to another stop (the C7a-1 review): the
// stop's re-decide compares the reason too, so a row an operator made
// dormant since the pass refuses the idle stop as re-decided.
func TestIdleStopRedecidesItsReason(t *testing.T) {
	c := idleCase(t, idleCity(), time.Hour, contentUnread)
	it, ok := c.idleArm()
	if !ok {
		t.Fatal("fixture: want the idle stop")
	}
	it.Key = c.k
	row := c.w.Census.Rows[c.k].Info
	row.MetadataState = string(session.StateSuspended)
	rt := &txRuntime{Class: rtAlive, Same: true, Identity: runtimeIdentity{Known: true, SessionID: c.k.ID, Token: "tok-3"}}
	v := txView{It: it, World: c.w, Alloc: c.a, Row: row, RT: rt, Fence: txFence{Read: legAttach | legPending | legWork, Work: &txWork{Free: true}, Activity: &activityRead{Reportable: true, Last: censusNow.Add(-time.Hour)}}, Now: censusNow}
	if step := stopDecide(v); step.Refuse != causeRedecided {
		t.Fatalf("an operator-dormant row: %+v, want refused %q", step, causeRedecided)
	}
}

// Kills an idle stop's refusal that holds the row (v5.9s): an idle stop
// settles on its own key, which defers the idle stops alone; any other
// stop or kind on the row is admitted; the key prunes with its row.
func TestIdleBackoffPacesOnlyTheIdleStop(t *testing.T) {
	p := newPlanner(realPlannerClock{}, func() time.Duration { return time.Minute }, nil, newInflightMap(), nil, io.Discard)
	k := admitRow(1)
	p.backoffSettled(settlement{Key: k, Kind: intentStop, Reason: decideIdleTimeout, Outcome: settledRefused, Cause: fenceAttached, At: admitT0})
	if p.backoff.Record(rowBackoffKey(k)).Consecutive != 0 || !p.backoff.Record(idleBackoffKey(k)).live(admitT0) {
		t.Fatalf("an idle refusal backed off %v, want its idle key alone", p.backoff.Snapshot())
	}
	in := admitIn(50, 50)
	in.Backoff = p.backoff.Snapshot()
	for _, it := range []intent{{Kind: intentStop, Reason: decideIdleTimeout}, {Kind: intentStop, Reason: decideIdleExhausted}, {Kind: intentStop, Reason: decideDormantStop}, {Kind: intentStart}} {
		it.Key = k
		res := admit(in, []intent{it})
		held := len(res.Deferred) == 1 && res.Deferred[0].Cause == causeRowBackoff
		if want := isIdleReason(it.Reason); held != want {
			t.Errorf("(%s, %s): deferred on the idle backoff %v, want %v", it.Kind, it.Reason, held, want)
		}
	}
}

// Kills an idle stop's L5 wider or laxer than legacy's late claim (v5.9s,
// SESS-574): an open bead does not hold it, an in-progress claim does,
// and a work leg gone dark counts as work.
func TestIdleStopReadsTheLateClaim(t *testing.T) {
	idle := intent{Kind: intentStop, Reason: decideIdleTimeout}
	for _, tc := range []struct {
		name   string
		status string
		dark   bool
		free   bool
	}{{"an open bead", "open", false, true}, {"an in-progress claim", "in_progress", false, false}, {"a dark leg", "", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGatherFixture(t, poolRow("gc-1", "worker", 1, "active"))
			work := beads.NewMemStore()
			if tc.status != "" {
				b, err := work.Create(beads.Bead{Title: "w", Type: "task", Assignee: "gc-1"})
				if err == nil {
					err = work.Update(b.ID, beads.UpdateOpts{Status: stringPtr(tc.status)})
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			var store beads.Store = work
			if tc.dark {
				store = failingListStore{work}
			}
			f.env.WorkStore = func() beads.Store { return store }
			w := f.gather(t)
			if got := newEffectPass(&w, &allocDecision{}).readWork(w.Census.Canonical()[0].Info, idle); got.Free != tc.free {
				t.Fatalf("L5 = %+v, want free %v", got, tc.free)
			}
		})
	}
}

// Kills a stop record legacy emits and v2 drops (mc-ensxp), one emitted
// every pass, and one whose codes are not the decision's (#7791 review): a
// decided idle stop is recorded at the idle-timeout site with the codes
// legacy takes through timerTraceCodes, idle_timeout/stop, or for the
// forced stop assigned_work_exhausted/stop_defer_exhausted, again only a
// patrol later or on another reason; any other stop is not.
func TestIdleMemoryTracesStops(t *testing.T) {
	const patrol = 15 * time.Second
	var m idleMemory
	c := idleCase(t, idleCity(), time.Hour, contentUnread)
	m.update(c.w, c.a, patrol, io.Discard)
	exhaust := func(on bool) {
		e := c.a.Snapshot.Entries[c.k]
		e.AssignedWork, c.w.Idle.defers[c.k] = nil, 0
		if on { // assigned work deferred past the streak's limit: the forced stop
			e.AssignedWork, c.w.Idle.defers[c.k] = &assignedWorkView{BeadID: "gw-1"}, 9
		}
	}
	idle, exhausted := intent{Key: c.k, Kind: intentStop, Reason: decideIdleTimeout}, intent{Key: c.k, Kind: intentStop, Reason: decideIdleExhausted}
	for i, step := range []struct {
		at        time.Duration
		it        intent
		exhausted bool
		want      string
	}{
		{0, idle, false, "idle_timeout/stop"},
		{time.Second, idle, false, ""},
		{2 * time.Second, exhausted, true, "assigned_work_exhausted/stop_defer_exhausted"},
		{2*time.Second + patrol, exhausted, true, "assigned_work_exhausted/stop_defer_exhausted"},
		{time.Minute, intent{Key: c.k, Kind: intentStop, Reason: decideDormantStop}, false, ""},
		{2 * time.Minute, intent{Key: c.k, Kind: intentStart, Reason: decideIdleTimeout}, false, ""},
	} {
		c.w.Now = censusNow.Add(step.at)
		exhaust(step.exhausted)
		var got string
		for _, tr := range m.stopTraces(c.w, c.a, []intent{step.it}, patrol) {
			if tr.Site != TraceSiteReconcilerIdleTimeout || tr.Template != "worker" || tr.SessionName != "s-heal" || tr.Key != c.k {
				t.Fatalf("step %d: trace %+v, want the idle-timeout site for worker/s-heal", i, tr)
			}
			got = tr.Reason + "/" + tr.Outcome
		}
		if got != step.want {
			t.Fatalf("step %d: traced %q, want %q", i, got, step.want)
		}
	}
}

// openListCounter is a work store that counts the lists of open beads, the
// finalize's close-gate read (the fence's late claim lists in_progress only).
type openListCounter struct {
	beads.Store
	open *atomic.Int64
}

func (s openListCounter) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Status == "open" {
		s.open.Add(1)
	}
	return s.Store.List(q)
}

// Kills an idle stop's finalize that reads work it never uses (mc-ensxp):
// the idle stop and the forced stop land with no close-gate work read, and
// another stop's finalize still reads it.
func TestIdleStopFinalizeReadsNoWork(t *testing.T) {
	for _, exhausted := range []bool{false, true} {
		p, it, store, _ := idleOn(t, exhausted)
		var open atomic.Int64
		p.reads.legs = testWorkLegs(p.World.CityPath, p.World.Env.Cfg, openListCounter{store, &open}, nil).readOnly()
		if s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil); s.Outcome != settledLanded || open.Load() != 0 {
			t.Fatalf("%s: settled %+v with %d open-work lists, want landed with none", it.Reason, s, open.Load())
		}
	}
	var open atomic.Int64
	r := effectReads{legs: testWorkLegs(t.TempDir(), idleCity(), openListCounter{beads.NewMemStore(), &open}, nil).readOnly()}
	c := idleCase(t, idleCity(), time.Hour, contentUnread)
	v := txView{It: intent{Key: c.k, Kind: intentStop, Reason: decideStopVerb + drainOrphaned}, World: c.w, Row: c.w.Census.Rows[c.k].Info, Now: censusNow}
	if _, err := finalizeWorkProbe(context.Background(), r, v); err != nil || open.Load() == 0 {
		t.Fatalf("an orphaned stop's finalize: err %v, %d open-work lists; want its close-gate read", err, open.Load())
	}
}

// Kills a pass that drops A15's stop records (mc-ensxp), or records them
// under codes other than the decision's (#7791 review): a simulated seat
// idle past its idle_timeout is stopped, and the pass that decides the stop
// records it at the idle-timeout site, idle_timeout/stop for a seat with no
// work, assigned_work_exhausted/stop_defer_exhausted for one whose assigned
// work defers it past its limit.
func TestPassRecordsTheIdleStop(t *testing.T) {
	for _, c := range []struct {
		name   string
		noWork bool
		want   string
	}{
		{"no work", true, "idle_timeout/stop"},
		{"assigned work past its limit", false, "assigned_work_exhausted/stop_defer_exhausted"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := respawnSim(t)
			s.cfg.Agents[0].IdleTimeout = "30s"
			if c.noWork { // routed demand keeps the seat wanted; no work is assigned to it
				empty := ""
				if err := s.legs[0].backing.Update("gw-1", beads.UpdateOpts{Assignee: &empty}); err != nil {
					t.Fatal(err)
				}
				s.legs[0].cache.ReconcileNowForTest()
			}
			s.sp.mu.Lock()
			s.sp.SetActivity("s-gc-1", s.clk.Now().Add(-10*time.Minute))
			s.sp.mu.Unlock()
			var stops []rowTrace
			pass := s.p.pass
			s.p.pass = func(now time.Time) passResult {
				res := pass(now)
				for _, tr := range s.p.out.record.Load().IdleTraces {
					if strings.HasPrefix(tr.Outcome, string(TraceOutcomeStop)) {
						stops = append(stops, tr)
					}
				}
				return res
			}
			s.tickUntil(t, 20, "recorded an idle stop", func() bool { return len(stops) > 0 })
			if tr := stops[0]; tr.Site != TraceSiteReconcilerIdleTimeout || tr.SessionName != "s-gc-1" || tr.Reason+"/"+tr.Outcome != c.want {
				t.Fatalf("stop record %+v, want %s at the idle-timeout site for s-gc-1", tr, c.want)
			}
			s.noViolations(t)
		})
	}
}
