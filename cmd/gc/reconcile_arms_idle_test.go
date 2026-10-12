package main

import (
	"context"
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

// Kills a ladder out of legacy's order (v5 A15, SESS-574/575/576): a live
// desired row idle past its idle_timeout, by its activity or its content
// clock, is stopped; one with no timeout, recent or unknown activity, a
// lifecycle blocker, a min-floor seat, assigned work within its defer
// limit, or a runtime not its own is not; work past the limit is the
// forced stop.
func TestArmIdleTimeout(t *testing.T) {
	work := func(c *healCase) { c.a.Snapshot.Entries[c.k].AssignedWork = &assignedWorkView{BeadID: "gw-1"} }
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
		{name: "the content clock fired", cfg: idleCity(), idle: -1, setup: func(c *healCase) { c.w.ContentIdle = map[rowKey]bool{c.k: true} }, want: decideIdleTimeout},
		{name: "no idle_timeout", cfg: workerCity(3), idle: time.Hour},
		{name: "a user hold", cfg: idleCity(), idle: time.Hour, meta: []string{"held_until", rowAt(time.Hour)}},
		{name: "a pin", cfg: idleCity(), idle: time.Hour, meta: []string{"pin_awake", "true"}},
		{name: "not desired", cfg: idleCity(), idle: time.Hour, setup: func(c *healCase) { c.a.Snapshot.Entries[c.k].InDesired = false }},
		{name: "assigned work within the limit", cfg: idleCity(), idle: time.Hour, setup: func(c *healCase) { work(c); c.w.IdleDefers = map[rowKey]int{c.k: 3} }},
		{name: "assigned work past the limit", cfg: idleCity(), idle: time.Hour, setup: func(c *healCase) { work(c); c.w.IdleDefers = map[rowKey]int{c.k: 4} }, want: decideIdleExhausted},
		{name: "a configured limit", cfg: idleCity(func(a *config.Agent) { a.AssignedWorkDeferLimit = intPtr(1) }), idle: time.Hour, setup: func(c *healCase) { work(c); c.w.IdleDefers = map[rowKey]int{c.k: 2} }, want: decideIdleExhausted},
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

// Kills a defer streak legacy would not count (SESS-575) and a content
// anchor that fires early, stays across a busy read, or outlives its
// incarnation (ga-07mi8): a deferred row counts on, a new anchor bead
// restarts at one, a pass without the defer forgets it; an idle pane
// anchors, fires past the timeout (and re-anchors), a busy one clears.
func TestIdleMemoryUpdate(t *testing.T) {
	t.Run("defer streaks", func(t *testing.T) {
		var m idleMemory
		for i, step := range []struct {
			bead string // the row's current bead; "" for no assigned work
			want int
		}{{"gw-1", 1}, {"gw-1", 2}, {"gw-2", 1}, {"", 0}, {"gw-2", 1}} {
			c := idleCase(t, idleCity(), time.Hour, contentUnread, session.CurrentBeadIDKey, step.bead)
			if step.bead != "" {
				c.a.Snapshot.Entries[c.k].AssignedWork = &assignedWorkView{BeadID: step.bead}
			}
			defers, _, _ := m.update(c.w, c.a)
			if defers[c.k] != step.want {
				t.Fatalf("pass %d: streak %d, want %d", i, defers[c.k], step.want)
			}
		}
	})
	t.Run("content anchors", func(t *testing.T) {
		var m idleMemory
		claude := []string{"provider", "claude"}
		for i, step := range []struct {
			at      time.Duration
			content contentState
			fires   bool
		}{
			{0, contentIdle, false},
			{4 * time.Minute, contentIdle, false},
			{6 * time.Minute, contentError, false}, // an erring read keeps the anchor, fires nothing
			{6 * time.Minute, contentIdle, true},
			{7 * time.Minute, contentIdle, false}, // re-anchored
			{8 * time.Minute, contentBusy, false},
			{13 * time.Minute, contentIdle, false}, // the busy read cleared the anchor
		} {
			c := idleCase(t, idleCity(), -1, step.content, claude...)
			c.w.Now = censusNow.Add(step.at)
			c.w.ObsMaxAge = time.Hour
			_, fired, want := m.update(c.w, c.a)
			if fired[c.k] != step.fires || c.w.ContentIdle[c.k] != step.fires || !want["s-heal"] {
				t.Fatalf("pass %d: fired %v (World %v), want %v; content wanted %v", i, fired[c.k], c.w.ContentIdle[c.k], step.fires, want)
			}
		}
		if len(m.since) != 1 {
			t.Fatalf("anchors %v, want the one live incarnation's", m.since)
		}
		c := idleCase(t, idleCity(), -1, contentIdle, claude...)
		c.a.Snapshot.Entries[c.k].Liveness = livenessGone
		if _, _, want := m.update(c.w, c.a); len(m.since) != 0 || len(want) != 0 {
			t.Fatalf("a gone row: anchors %v, wanted %v; want none", m.since, want)
		}
	})
	t.Run("off the content clock", func(t *testing.T) {
		var m idleMemory
		c := idleCase(t, idleCity(), -1, contentIdle)
		if _, _, want := m.update(c.w, c.a); len(m.since) != 0 || len(want) != 0 {
			t.Fatalf("a non-claude row: anchors %v, wanted %v; want none", m.since, want)
		}
	})
}

// Kills a lifecycle setting resolved other than legacy's trackers resolve
// it: a named session's own agent's (but never an always one's), a pool
// template's to its every row, a singleton's to its one runtime name.
func TestLifecycleSetting(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(3), IdleTimeout: "1m"},
			{Name: "solo", StartCommand: "true", MaxActiveSessions: intPtr(1), IdleTimeout: "2m"},
			{Name: "chat", StartCommand: "true", IdleTimeout: "3m"},
			{Name: "boss", StartCommand: "true", IdleTimeout: "4m"},
		},
		NamedSessions: []config.NamedSession{{Template: "chat", Mode: "on_demand"}, {Template: "boss", Mode: "always"}},
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
	} {
		if got := idleTimeoutFor(cfg, tc.template, tc.name); got != tc.want {
			t.Errorf("idleTimeoutFor(%q, %q) = %v, want %v", tc.template, tc.name, got, tc.want)
		}
	}
}

// idleOn decides an idle row on admittedBeginOn's leaf, its content clock
// fired and its defer streak past any limit, its identity metadata settled
// first (the probe's A7 patch seeded), so A15 decides it.
func idleOn(t *testing.T, exhausted bool) (*effectPass, intent, beads.Store, *fenceLeaf) {
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
		w.ContentIdle, w.IdleDefers = map[rowKey]bool{}, map[rowKey]int{}
		for _, row := range w.Census.Rows {
			w.Templates.entries[templateMemoKeyOf(row.Info)] = templateResolution{TP: TemplateParams{TemplateName: "worker"}}
			w.ContentIdle[row.Key], w.IdleDefers[row.Key] = true, 9
		}
	}
	probe, it, _, _ := admittedBeginOn(t, nil, entry, world)
	var meta []string
	if it.Kind == intentRowMetadata {
		row := probe.World.Census.Rows[it.Key]
		id, blocked, _, _ := probe.World.rowIdentityOf(it.Key, row.Info, row.Refresh)
		for k, v := range identityPatch(id, row.Info, blocked, nil, nil, "") {
			meta = append(meta, k, v)
		}
	}
	return admittedBeginOn(t, meta, entry, world)
}

// Kills an idle stop that ignores a pending interaction or a claim since
// the pass, or loses legacy's side effects (SESS-574/575/576): the idle
// stop refuses on either; the forced stop stops over the claim; a landed
// idle stop clears the pane's scrollback, emits session.idle_killed and
// sleeps the row with its reason.
func TestIdleStopThroughTheEffectPass(t *testing.T) {
	for _, tc := range []struct {
		name      string
		exhausted bool
		set       func(*testing.T, *fenceLeaf, beads.Store, string)
		landed    bool
	}{
		{name: "idle", landed: true},
		{name: "a pending interaction", set: func(_ *testing.T, l *fenceLeaf, _ beads.Store, _ string) {
			l.SetPendingInteraction("s-gc-1", &runtime.PendingInteraction{RequestID: "r", Kind: "approval"})
		}},
		{name: "a claim since the pass", set: func(t *testing.T, _ *fenceLeaf, s beads.Store, id string) { assignWork(t, s, id) }},
		{name: "exhausted, over a claim", exhausted: true, set: func(t *testing.T, _ *fenceLeaf, s beads.Store, id string) { assignWork(t, s, id) }, landed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, it, store, leaf := idleOn(t, tc.exhausted)
			want := map[bool]string{false: decideIdleTimeout, true: decideIdleExhausted}[tc.exhausted]
			if it.Kind != intentStop || it.Reason != want {
				t.Fatalf("decided (%q, %q) %v, want the stop for %q", it.Kind, it.Reason, it.Patch, want)
			}
			if tc.set != nil {
				tc.set(t, leaf, store, it.Key.ID)
			}
			s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
			got, _ := store.Get(it.Key.ID)
			if !tc.landed {
				if s.Outcome != settledRefused || stops(leaf) != 0 || got.Metadata["state"] == "asleep" {
					t.Fatalf("settled %+v, stops %d, state %q; want refused, nothing stopped", s, stops(leaf), got.Metadata["state"])
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
