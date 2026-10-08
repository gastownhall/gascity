package main

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/resilience"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worktree"
)

// The start effect's Launch tests (CONTRACT v5 S1, S2, D1, I15, I24; plan
// C5a1): PreWake, prepare, the FreshOnly Start and the post-call check.

// runIn runs kind's effect on the row under ctx.
func (f *startFixture) runIn(ctx context.Context, t *testing.T, kind string) settlement {
	t.Helper()
	it := intent{Kind: kind, Key: f.key, Deadline: f.clk.Now().Add(70 * time.Second)}
	return effectRegistry[kind](f.pass(t), it)(ctx)
}

// stubTranscript makes every keyed transcript present or absent.
func stubTranscript(t *testing.T, present bool) {
	t.Helper()
	old := staleResumeKeyProbe
	staleResumeKeyProbe = func(string, string, string) (bool, bool) { return present, true }
	t.Cleanup(func() { staleResumeKeyProbe = old })
}

// Kills a launch that commits a token the runtime does not carry, a commit
// without session.woke or with another payload, and a reused incarnation:
// a gone runtime (as a confirmed-dead tmux server reads, D-19) launches
// under a new generation and token, and the runtime carries the token the
// commit writes.
func TestLaunchCommitsThePreWakeToken(t *testing.T) {
	for _, backend := range rowWriteBackends {
		t.Run(backend.name, func(t *testing.T) {
			f := newStartFixture(t, backend.open(t))
			s := f.run(t, intentStart)
			m := f.meta(t)
			want := events.Event{Type: events.SessionWoke, Actor: "gc", Subject: f.tp.DisplayName(), SessionID: f.key.ID}
			if s.Outcome != settledLanded || s.Event == nil || !reflect.DeepEqual(*s.Event, want) || s.Noted == nil {
				t.Fatalf("settlement %+v (event %+v), want landed, noted, with %+v", s, s.Event, want)
			}
			if m["generation"] != "4" || m["instance_token"] == "tok" || m["state"] != "active" || m["started_config_hash"] == "" {
				t.Fatalf("row after the launch = %v, want generation 4, a new token, active, hashed", m)
			}
			if len(f.leaf.starts) != 1 || f.leaf.starts[0].Env["GC_INSTANCE_TOKEN"] != m["instance_token"] {
				t.Fatalf("started %d runtimes, the first with token %q; want one carrying %q", len(f.leaf.starts), f.leaf.starts[0].Env["GC_INSTANCE_TOKEN"], m["instance_token"])
			}
		})
	}
}

// Kills a v2 start that reaches an adapter recycle, killExistingOrphans or
// warm reuse (I24): every provider Start the effect makes, a held name's
// included, carries FreshOnly, and a held name is never stopped.
func TestEveryV2StartUsesFreshOnly(t *testing.T) {
	f := newStartFixture(t, requireMem(t))
	f.run(t, intentStart)
	held := newStartFixture(t, requireMem(t))
	held.leaf.startErr = runtime.ErrSessionExists
	held.leaf.onStart = func(context.Context) {
		held.leaf.live["s-a"], held.leaf.env["s-a"] = liveAlive, map[string]string{"GC_SESSION_ID": "other", "GC_INSTANCE_TOKEN": "x"}
	}
	if s := held.run(t, intentStart); s.Outcome != settledRefused || s.Cause != causeOccupied {
		t.Fatalf("held by another row: %+v, want refused occupied (START-018)", s)
	}
	for _, l := range []*startLeaf{f.leaf, held.leaf} {
		if len(l.starts) != 1 || !l.starts[0].FreshOnly {
			t.Fatalf("starts %+v, want one with FreshOnly", l.starts)
		}
		for _, c := range l.SnapshotCalls() {
			if c.Method == "Stop" {
				t.Fatal("the effect stopped a runtime")
			}
		}
	}
}

// Kills a stop request that survives a v2 PreWake (I15; v5 D1): both halves
// are cleared in the PreWake CAS.
func TestPreWakeClearsStopRequest(t *testing.T) {
	var keys, meta []string
	for k := range stopVoidResiduePatch() {
		keys, meta = append(keys, k), append(meta, k, "set")
	}
	if len(keys) != 5 {
		t.Fatalf("the stop request has %d keys, want both halves' five", len(keys))
	}
	f := newStartFixture(t, requireMem(t), meta...)
	if s := f.run(t, intentStart); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
	for _, k := range keys {
		if v := f.meta(t)[k]; v != "" {
			t.Errorf("%s = %q after PreWake, want cleared", k, v)
		}
	}
}

// Kills a PreWake that does not re-decide on the fresh row: a hold, a kill
// fence, another generation or token, or an operator's wake request (a
// lifecycle fact the pass did not see) landing after the fresh read refuses
// with cause redecided, starts nothing and writes no PreWake.
func TestPreWakeRedecidesOnFreshRow(t *testing.T) {
	for name, write := range map[string]map[string]string{
		"held":       {"held_until": startT0.Add(time.Hour).Format(time.RFC3339)},
		"quarantine": {"quarantined_until": startT0.Add(time.Hour).Format(time.RFC3339)},
		"kill fence": {"state": "asleep", "state_reason": session.KillPendingReason, "sleep_reason": string(session.SleepReasonKilled), "slept_at": startT0.Format(time.RFC3339)},
		"rewoken":    {"generation": "4"},
		"retokened":  {"instance_token": "other"},
		"woken":      {"wake_request": "operator"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newStartFixture(t, requireMem(t))
			f.leaf.onRead = func() { f.set(t, write) }
			if s := f.run(t, intentStart); s.Outcome != settledRefused || s.Cause != causeRedecided {
				t.Fatalf("settlement %+v, want refused redecided", s)
			}
			if m := f.meta(t); len(f.leaf.starts) != 0 || m["last_woke_at"] != "" {
				t.Fatalf("started %d, row %v; want nothing started or written", len(f.leaf.starts), m)
			}
		})
	}
}

// Kills a resumed start whose immediate death is committed, and a post-call
// read taken before the stale-key wait (START-019): the read after a start
// with a session_key waits staleKeyDetectDelay on the effect's clock, and a
// runtime dead by then settles as died during startup, uncommitted.
func TestResumedStartWaitsStaleKeyDetectDelay(t *testing.T) {
	stubTranscript(t, true)
	f := newStartFixture(t, requireMem(t), "session_key", "k-1")
	f.tp.WorkDir = t.TempDir()
	f.leaf.afterStart = &liveCorpse
	done := make(chan settlement, 1)
	go func() { done <- f.run(t, intentStart) }()
	f.clk.waitTimerAt(t, startT0.Add(staleKeyDetectDelay))
	f.leaf.mu.Lock()
	reads := len(f.leaf.since)
	f.leaf.mu.Unlock()
	if reads != 2 {
		t.Fatalf("%d fresh reads before the wait ended, want only the pre-launch read and its absence check", reads)
	}
	f.clk.Advance(staleKeyDetectDelay)
	s := <-done
	if s.Outcome != settledFailed || s.Cause != causeDiedDuringStartup {
		t.Fatalf("settlement %+v, want failed died-during-startup", s)
	}
	if m := f.meta(t); m["started_config_hash"] != "" || m["state"] != "creating" || m["session_key"] != "k-1" {
		t.Fatalf("row %v, want uncommitted with its key (C5a2's abandon clears it)", m)
	}
}

// waitTimerAt waits until some timer is armed for want.
func (c *fakePlannerClock) waitTimerAt(t *testing.T, want time.Time) {
	t.Helper()
	guard := time.After(plannerTestGuard)
	for {
		c.mu.Lock()
		for _, tm := range c.timers {
			if tm.active && tm.at.Equal(want) {
				c.mu.Unlock()
				return
			}
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-changed:
		case <-guard:
			t.Fatalf("no timer armed for %s", want)
		}
	}
}

// Kills a commit, or a silent success, over an agent that died at once: a
// Start that returned nil over a runtime the post-call read finds gone is
// died during startup.
func TestGoneAfterSuccessfulStartIsDiedDuringStartup(t *testing.T) {
	f := newStartFixture(t, requireMem(t))
	f.leaf.afterStart = &runtime.Liveness{}
	if s := f.run(t, intentStart); s.Outcome != settledFailed || s.Cause != causeDiedDuringStartup || f.meta(t)["started_config_hash"] != "" {
		t.Fatalf("settlement %+v, want failed died-during-startup, uncommitted", s)
	}
}

// Kills a relaunch loop, and a held dead name taken for a start: a dead
// runtime that took the name after the fresh read, so Start returned
// ErrSessionExists, refuses with cause held-dead (a row backoff) after one
// Start. C5a3 adds the recycle and the one relaunch.
func TestErrSessionExistsOnDeadNameRefusesHeldDead(t *testing.T) {
	f := newStartFixture(t, requireMem(t))
	f.leaf.startErr = runtime.ErrSessionExists
	f.leaf.onStart = func(context.Context) { f.leaf.live["s-a"] = liveCorpse }
	if s := f.run(t, intentStart); s.Outcome != settledRefused || s.Cause != causeHeldDead || len(f.leaf.starts) != 1 {
		t.Fatalf("settlement %+v after %d starts, want held-dead after one", s, len(f.leaf.starts))
	}
}

// Kills a commit after the effect's deadline: a Start that returns after it
// commits nothing and settles as a deadline by context.Cause, leaving the
// runtime for the next pass to adopt (v5 S7).
func TestLaunchPastDeadlineCommitsNothing(t *testing.T) {
	f := newStartFixture(t, requireMem(t))
	ctx, cancel := context.WithCancelCause(context.Background())
	f.leaf.onStart = func(context.Context) { cancel(context.DeadlineExceeded) }
	if s := f.runIn(ctx, t, intentStart); s.Outcome != settledFailed || s.Cause != causeDeadline {
		t.Fatalf("settlement %+v, want failed at the deadline", s)
	}
	if m := f.meta(t); m["started_config_hash"] != "" || m["state"] != "creating" {
		t.Fatalf("row %v, want PreWaked and uncommitted", m)
	}
}

// Kills legacy's blind clear and re-mint behind the refusing store (S-1):
// the PreWake CAS fills an empty key once and replaces a key whose
// transcript is gone, and prepare launches that key without writing.
func TestStartEffectFreshKeyNotClearedOrReminted(t *testing.T) {
	for _, c := range []struct {
		name string
		meta []string
	}{{"empty key filled", nil}, {"stale key replaced", []string{"session_key", "stale"}}} {
		t.Run(c.name, func(t *testing.T) {
			stubTranscript(t, false)
			f := newStartFixture(t, requireMem(t), c.meta...)
			f.clk.auto = true // the stale-key wait
			f.tp.WorkDir = t.TempDir()
			f.tp.ResolvedProvider = &config.ResolvedProvider{Name: "claude", SessionIDFlag: "--session-id", ResumeFlag: "--resume"}
			if s := f.run(t, intentStart); s.Outcome != settledLanded {
				t.Fatalf("settlement %+v, want landed", s)
			}
			key := f.meta(t)["session_key"]
			if key == "" || key == "stale" || !strings.HasSuffix(f.leaf.starts[0].Command, "--session-id "+key) {
				t.Fatalf("row key %q, command %q; want a fresh key launched with --session-id", key, f.leaf.starts[0].Command)
			}
		})
	}
}

// lockedFor reports whether id's session mutation lock stays held for a
// moment; the waiter it starts takes it later and lets it go.
func lockedFor(id string) bool {
	got := make(chan struct{})
	go func() { _ = session.WithSessionMutationLock(id, func() error { close(got); return nil }) }()
	select {
	case <-got:
		return false
	case <-time.After(100 * time.Millisecond):
		return true
	}
}

// Kills a session lock held across the provider Start (S-13: a Manager path
// would wait out a whole start, and the commit self-deadlocks), and a
// section split that lets an API submit land between a read and its write
// (a pending create turned StaleSelf): the row's session mutation lock is
// held from the fresh read through PreWake and from the post-call read
// through the commit, and free while the provider starts.
func TestLaunchMutationLockSections(t *testing.T) {
	f := newStartFixture(t, requireMem(t))
	var held []bool
	f.leaf.onRead = func() { held = append(held, lockedFor(f.key.ID)) }
	f.leaf.onStart = func(context.Context) {
		if lockedFor(f.key.ID) {
			t.Error("the session mutation lock was held across the provider Start")
		}
	}
	if s := f.run(t, intentStart); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
	if len(held) < 2 || slices.Contains(held, false) {
		t.Fatalf("lock held at each fresh read = %v, want held at every one", held)
	}
}

// Kills a Noop that takes a half-open endpoint's single probe (SC N1): the
// ticket is taken only by a launch, which holds it through the Start.
func TestEndpointTicketOnlyOnLaunch(t *testing.T) {
	now := startT0
	guard := newEndpointCapacityGuard(func() time.Time { return now })
	const k endpointKey = "upstream:e"
	trip, _ := guard.Admit(k, "outside", "outside")
	trip.Resolve(verdictCapacity)
	now = now.Add(guard.breaker(k).Status().BackoffCap)
	probeFree := func() bool {
		ticket, ok := guard.Admit(k, "probe", "probe")
		ticket.Resolve(verdictNotAttempted)
		return ok
	}
	noop := newStartFixture(t, requireMem(t), "state", "active")
	noop.guard, noop.tp.Upstream = guard, "e"
	noop.leaf.runtimeAs(liveAlive, noop.key.ID, "tok")
	noop.leaf.onRead = func() {
		if !probeFree() {
			t.Error("a Noop took the endpoint's probe")
		}
	}
	if s := noop.run(t, intentStart); s.Outcome != settledNoop {
		t.Fatalf("settlement %+v, want a Noop", s)
	}
	launch := newStartFixture(t, requireMem(t))
	launch.guard, launch.tp.Upstream = guard, "e"
	launch.leaf.onStart = func(context.Context) {
		if probeFree() {
			t.Error("the launch started without the endpoint's probe")
		}
	}
	if s := launch.run(t, intentStart); s.Outcome != settledLanded || guard.breaker(k).Status().State != resilience.StateClosed {
		t.Fatalf("settlement %+v, breaker %v; want landed and the probe's success closing it", s, guard.breaker(k).Status().State)
	}
}

// Kills a binding dropped from PreWake, and a worktree applied unverified
// (POOL-055, #34): the pass's binding patch and the verified work dir land
// in the PreWake CAS; refused evidence refuses the start before PreWake,
// with the work item's verdict.
func TestPreWakeFoldsVerifiedBinding(t *testing.T) {
	spec := &worktree.Spec{BeadID: "w-1"}
	for _, verifyErr := range []error{nil, errors.New("not owned")} {
		f := newStartFixture(t, requireMem(t))
		f.alloc.Snapshot.Entries[f.key] = &selectionEntry{Binding: &bindingTarget{WorktreeSpec: spec, Patch: session.MetadataPatch{beadmeta.TriggerBeadIDMetadataKey: "w-1"}}}
		p := f.pass(t)
		it := intent{Kind: intentStart, Key: f.key, Deadline: f.clk.Now().Add(time.Minute)}
		e := startEffect{pass: p, it: it, verify: func(worktree.Spec) (worktree.Report, error) { return worktree.Report{Path: "/wt/w-1"}, verifyErr }}
		s, m := e.run(context.Background()), f.meta(t)
		switch {
		case s.Work == nil || s.Work.Refused != (verifyErr != nil):
			t.Fatalf("verify %v: work verdict %+v", verifyErr, s.Work)
		case verifyErr == nil && (m[beadmeta.TriggerBeadIDMetadataKey] != "w-1" || m[beadmeta.WorkDirMetadataKey] != "/wt/w-1"):
			t.Fatalf("row %v, want the binding and the verified work dir", m)
		case verifyErr != nil && (s.Cause != causeWorktree || m["generation"] != "3"):
			t.Fatalf("refused evidence: settlement %+v, row %v; want refused before PreWake", s, m)
		}
	}
}

// acpLeaf is a startLeaf that records ACP routes, as the auto composite
// registers them.
type acpLeaf struct {
	*startLeaf
	routed []string
}

func (l *acpLeaf) RouteACP(name string) { l.routed = append(l.routed, name) }

// Kills an ACP launch started before its route exists: the effect routes the
// name to ACP before it reads or starts.
func TestACPLaunchRoutesBeforeStart(t *testing.T) {
	f := newStartFixture(t, requireMem(t))
	f.tp.IsACP = true
	leaf := &acpLeaf{startLeaf: f.leaf}
	f.leaf.onStart = func(context.Context) {
		if len(leaf.routed) != 1 || leaf.routed[0] != "s-a" {
			t.Errorf("routes at Start = %v, want s-a routed to ACP", leaf.routed)
		}
	}
	p := f.pass(t)
	p.Runtime = leaf
	if s := bringUpEffect(p, intent{Kind: intentStart, Key: f.key, Deadline: f.clk.Now().Add(time.Minute)})(context.Background()); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
}

// Kills a launch commit that drops legacy's MCP keys or the #46 clear: the
// commit writes the snapshot and identity, zeroes the row's #46 mirror, and
// clears the accrued episode, with no blind write to the session row.
func TestLaunchCommitCarriesMCPAndClearsStartupHealth(t *testing.T) {
	f := newStartFixture(t, requireMem(t), "agent_name", "worker-1", session.MCPServersSnapshotMetadataKey, "stale",
		startupHealthActiveCountMetadataKey, "2", startupHealthActiveKindMetadataKey, "crash")
	info := sessionInfoOf(t, f.store, f.key.ID)
	key := startupHealthEpisodeKey(info, "s-a")
	if err := sessionFrontDoor(f.store).SaveStartupHealthEpisode(session.StartupHealthEpisode{SessionName: key, ConsecutiveCount: 2}); err != nil {
		t.Fatal(err)
	}
	f.store = blindSpyStore{Store: f.store, t: t, row: f.key.ID}
	if s := f.run(t, intentStart); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
	m := f.meta(t)
	if m[session.MCPServersSnapshotMetadataKey] == "stale" || m[session.MCPIdentityMetadataKey] != "worker-1" ||
		m[startupHealthActiveCountMetadataKey] != "0" || m[startupHealthActiveKindMetadataKey] != "" {
		t.Fatalf("row after the commit = %v, want the MCP keys and a zeroed #46 mirror", m)
	}
	if ep, err := sessionFrontDoor(f.store).LoadStartupHealthEpisode(key); err != nil || ep.ConsecutiveCount != 0 {
		t.Fatalf("episode %+v (%v), want cleared", ep, err)
	}
}

// Kills the provider's own died-during-startup error taken as a plain start
// error: with the runtime gone, it is died during startup.
func TestProviderDiedDuringStartupIsDeath(t *testing.T) {
	f := newStartFixture(t, requireMem(t))
	f.leaf.startErr = runtime.ErrSessionDiedDuringStartup
	if s := f.run(t, intentStart); s.Outcome != settledFailed || s.Cause != causeDiedDuringStartup {
		t.Fatalf("settlement %+v, want failed died-during-startup", s)
	}
}

// Kills a provider Start not bounded at startup_timeout (the deadline less
// its 10s slack): its context ends, by deadline, once the clock reaches it.
func TestProviderStartBoundedAtStartupTimeout(t *testing.T) {
	f := newStartFixture(t, requireMem(t))
	f.leaf.onStart = func(ctx context.Context) {
		f.clk.Advance(70*time.Second - startDeadlineSlack)
		select {
		case <-ctx.Done():
			if !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
				t.Errorf("Start's context ended with %v, want its deadline", context.Cause(ctx))
			}
		case <-time.After(plannerTestGuard):
			t.Error("Start's context outlived startup_timeout")
		}
	}
	f.run(t, intentStart)
}

// Kills the deadline checks around the provider call: a context that ends
// while PreWake decides writes nothing, and one that ends during the
// post-call read commits nothing; each settles as a deadline.
func TestLaunchContextChecks(t *testing.T) {
	spec := &worktree.Spec{BeadID: "w-1"}
	f := newStartFixture(t, requireMem(t))
	f.alloc.Snapshot.Entries[f.key] = &selectionEntry{Binding: &bindingTarget{WorktreeSpec: spec}}
	ctx, cancel := context.WithCancelCause(context.Background())
	e := startEffect{
		pass: f.pass(t), it: intent{Kind: intentStart, Key: f.key, Deadline: f.clk.Now().Add(time.Minute)},
		verify: func(worktree.Spec) (worktree.Report, error) {
			cancel(context.DeadlineExceeded)
			return worktree.Report{}, nil
		},
	}
	if s := e.run(ctx); s.Outcome != settledFailed || s.Cause != causeDeadline || f.meta(t)["generation"] != "3" || len(f.leaf.starts) != 0 {
		t.Fatalf("ended during PreWake: settlement %+v, row %v; want a deadline, nothing written or started", s, f.meta(t))
	}

	f = newStartFixture(t, requireMem(t))
	ctx, cancel = context.WithCancelCause(context.Background())
	f.leaf.onIdentity = func() { cancel(context.DeadlineExceeded) }
	if s := f.runIn(ctx, t, intentStart); s.Outcome != settledFailed || s.Cause != causeDeadline || f.meta(t)["started_config_hash"] != "" {
		t.Fatalf("ended during the post-call read: settlement %+v; want a deadline, uncommitted", s)
	}
}

// Kills a launch under an empty session name (START-012): it refuses
// before the ticket and PreWake, writing nothing.
func TestLaunchRefusesEmptySessionName(t *testing.T) {
	f := newStartFixture(t, requireMem(t), "session_name", "")
	f.leaf.runtimeAs(runtime.Liveness{}, "", "")
	if s := f.run(t, intentStart); s.Outcome != settledRefused || s.Cause != causeSessionName || f.meta(t)["generation"] != "3" {
		t.Fatalf("settlement %+v, want refused %s and nothing written", s, causeSessionName)
	}
}

// Kills an endpoint-gate refusal backed off as a row failure: it is a
// deferral the breaker paces, as a swap pause is.
func TestEndpointGateRefusalDoesNotBackOffRow(t *testing.T) {
	p := settlePlanner(newInflightMap())
	k := rowKey{Leg: rowLeg, ID: "a"}
	p.settlements.post(settlement{Key: k, Kind: intentStart, Outcome: settledRefused, Cause: causeEndpointGate})
	p.drainSettlements(plannerT0)
	if r, ok := p.backoff.Snapshot()[rowBackoffKey(k)]; ok {
		t.Fatalf("row backoff %+v after an endpoint-gate refusal, want none", r)
	}
}

// Kills a concrete-pool work_dir repair left to prepare, which fails after
// PreWake on every attempt (the generation climbs) or writes blind: the
// PreWake CAS carries the repair, and the launch starts in the concrete dir.
func TestPreWakeFoldsConcretePoolRepair(t *testing.T) {
	f, concreteDir := concretePoolFixture(t)
	f.store = blindSpyStore{Store: f.store, t: t, row: f.key.ID}
	if s := f.run(t, intentStart); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
	m := f.meta(t)
	if m[beadmeta.WorkDirMetadataKey] != concreteDir || m[beadmeta.LegacyWorkDirMetadataKey] != concreteDir || m["generation"] != "4" || f.leaf.starts[0].WorkDir != concreteDir {
		t.Fatalf("row %v, started in %q; want the repair in one PreWake and the concrete dir", m, f.leaf.starts[0].WorkDir)
	}
}

// Kills a launch blind to the task it was woken for: an agent woken mid-task
// probes its key and starts in the task's checkout, from the pass's
// assigned work, unless that read was partial (legacy's resolver).
func TestLaunchUsesAssignedTaskWorkDir(t *testing.T) {
	for _, partial := range []bool{false, true} {
		f := newStartFixture(t, requireMem(t), "session_key", "k-1")
		f.clk.auto = true // the stale-key wait
		taskDir := t.TempDir()
		f.tp.WorkDir = t.TempDir()
		f.demand = demandView{StorePartial: partial, AssignedWork: []beads.Bead{{ID: "w-1", Status: "in_progress", Assignee: f.key.ID, Metadata: map[string]string{"work_dir": taskDir}}}}
		var probed string
		old := staleResumeKeyProbe
		staleResumeKeyProbe = func(_, dir, _ string) (bool, bool) { probed = dir; return true, true }
		s := f.run(t, intentStart)
		staleResumeKeyProbe = old
		want := taskDir
		if partial {
			want = f.tp.WorkDir
		}
		if s.Outcome != settledLanded || probed != want || f.leaf.starts[0].WorkDir != want {
			t.Fatalf("partial %v: settlement %+v, probed %q, started in %q; want %q", partial, s, probed, f.leaf.starts[0].WorkDir, want)
		}
	}
}

// Kills a single-shot PreWake CAS: writers that land between each read and
// its CAS are outlasted within preWakeAttempts, and past it the start
// refuses prewake-contended, starting nothing.
func TestPreWakeRetriesContention(t *testing.T) {
	for _, c := range []struct {
		races  int
		landed bool
	}{{preWakeAttempts - 1, true}, {preWakeAttempts, false}} {
		f := newStartFixture(t, requireMem(t))
		inner, n := f.store, 0
		f.store = &getHookStore{Store: inner, onGet: func() {
			if n < c.races {
				n++
				if err := inner.SetMetadata(f.key.ID, "racer", strconv.Itoa(n)); err != nil {
					t.Error(err)
				}
			}
		}}
		s := f.run(t, intentStart)
		if got := s.Outcome == settledLanded; got != c.landed || (!got && (s.Cause != causePreWakeContended || len(f.leaf.starts) != 0)) {
			t.Fatalf("%d races: settlement %+v after %d starts, want landed=%v, else prewake-contended", c.races, s, len(f.leaf.starts), c.landed)
		}
	}
}

// Kills a provider Start in an ended context: a deadline that passes after
// PreWake lands settles as a deadline and never calls the provider.
func TestLaunchStartRefusesEndedContext(t *testing.T) {
	f := newStartFixture(t, requireMem(t), "session_key", "k-1")
	f.tp.WorkDir = t.TempDir()
	ctx, cancel := context.WithCancelCause(context.Background())
	old := staleResumeKeyProbe
	staleResumeKeyProbe = func(string, string, string) (bool, bool) { cancel(context.DeadlineExceeded); return true, true }
	t.Cleanup(func() { staleResumeKeyProbe = old })
	if s := f.runIn(ctx, t, intentStart); s.Outcome != settledFailed || s.Cause != causeDeadline || len(f.leaf.starts) != 0 {
		t.Fatalf("settlement %+v after %d starts, want a deadline and no Start", s, len(f.leaf.starts))
	}
}
