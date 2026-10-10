package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/resilience"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worktree"
)

// The start effect's Launch (CONTRACT v5 S1, S2, D1, I15, I24; plan C5a1) on
// the effect test kit: bringUp over an asleep row gc-1 (generation 3, token
// tok) whose runtime s-gc-1 the simulator's provider starts.

// startHookLeaf is the kit's leaf with a hook inside each Start, after RouteACP's
// record, and the ACP routes.
type startHookLeaf struct {
	*recordingLeaf
	onStart func(ctx context.Context)
	routed  []string
	cfgs    []runtime.Config // each Start's config
}

func (l *startHookLeaf) RouteACP(name string) { l.routed = append(l.routed, name) }

func (l *startHookLeaf) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if l.onStart != nil {
		l.onStart(ctx)
	}
	l.cfgs = append(l.cfgs, cfg)
	return l.recordingLeaf.Start(ctx, name, cfg)
}

// launchKit is the kit for a bringUp of row gc-1 in state with meta, its
// intent due 70s out, its provider a startHookLeaf.
func launchKit(t *testing.T, state string, meta ...string) (*txKit, *startHookLeaf) {
	t.Helper()
	k := startKit(t, intentStart, state, meta...)
	k.it.Deadline = gatherNow.Add(70 * time.Second)
	leaf := &startHookLeaf{recordingLeaf: k.leaf}
	k.p.Runtime = leaf
	return k, leaf
}

// starts is the simulator's provider Starts.
func (k *txKit) starts() []simStart {
	k.sp.mu.Lock()
	defer k.sp.mu.Unlock()
	return append([]simStart(nil), k.sp.starts...)
}

// mutate changes s-gc-1's runtime, or drops it with nil.
func (k *txKit) mutate(mark func(*simRuntime)) {
	k.sp.mu.Lock()
	defer k.sp.mu.Unlock()
	if mark == nil {
		k.sp.drop("s-gc-1")
		return
	}
	mark(k.sp.rts["s-gc-1"])
	k.sp.touch("s-gc-1")
}

// Kills a launch that commits a token the runtime does not carry, a commit
// without legacy's session.woke, a Start without FreshOnly (I24), and a
// reused incarnation: a gone runtime launches under generation 4 and a new
// token, which the started runtime carries and the commit writes.
func TestLaunchCommitsThePreWakeToken(t *testing.T) {
	k, _ := launchKit(t, "asleep")
	s := k.runKind()
	want := events.Event{Type: events.SessionWoke, Actor: "gc", Subject: (TemplateParams{TemplateName: "worker"}).DisplayName(), SessionID: "gc-1"}
	if s.Outcome != settledLanded || len(s.Facts.Events) != 1 || !reflect.DeepEqual(s.Facts.Events[0], want) || s.Facts.Noted == nil {
		t.Fatalf("settlement %+v, want landed, noted, with %+v", s, want)
	}
	token := k.meta("instance_token")
	if k.meta("generation") != "4" || token == "tok" || k.meta("state") != "active" || k.meta("started_config_hash") == "" {
		t.Fatalf("row: generation %q token %q state %q", k.meta("generation"), token, k.meta("state"))
	}
	if st := k.starts(); len(st) != 1 || !st[0].FreshOnly || st[0].Token != token || !st[0].Created {
		t.Fatalf("starts %+v, want one FreshOnly start carrying %q", st, token)
	}
}

// Kills a launch over a name taken after its reads (START-018): the Start's
// ErrSessionExists over another row's runtime refuses occupied, and the
// commit writes nothing.
func TestLaunchOverATakenNameRefuses(t *testing.T) {
	k, leaf := launchKit(t, "asleep")
	leaf.onStart = func(context.Context) { k.runtimeAs("gc-2", "theirs", nil) }
	if s := k.runKind(); s.Outcome != settledRefused || s.Cause != causeOccupied || k.meta("started_config_hash") != "" {
		t.Fatalf("settlement %+v, want refused occupied", s)
	}
	if st := k.starts(); len(st) != 1 || !st[0].FreshOnly || st[0].Created {
		t.Fatalf("starts %+v, want one FreshOnly start that created nothing", st)
	}
}

// Kills a stop request that survives a v2 PreWake (I15; v5 D1).
func TestPreWakeClearsStopRequest(t *testing.T) {
	var meta []string
	for k := range stopVoidResiduePatch() {
		meta = append(meta, k, "set")
	}
	k, _ := launchKit(t, "asleep", meta...)
	if s := k.runKind(); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
	for key := range stopVoidResiduePatch() {
		if v := k.meta(key); v != "" {
			t.Errorf("%s = %q after PreWake, want cleared", key, v)
		}
	}
}

// Kills a PreWake that does not decide again on the fresh row: a hold or
// kill fence the pass saw refuses (held); a hold, quarantine, kill fence,
// generation, token or wake request landing after the verb section refuses
// with the premise; each writes no PreWake and starts nothing.
func TestPreWakeRefusesWhatHoldsOrMovedSinceThePass(t *testing.T) {
	future := gatherNow.Add(time.Hour).Format(time.RFC3339)
	for _, c := range []struct {
		name   string
		meta   []string
		write  []string
		refuse string
	}{
		{"held at the pass", []string{"held_until", future}, nil, causeHeld},
		{"user-hold at the pass", []string{"sleep_intent", "user-hold"}, nil, causeHeld},
		{"quarantined at the pass", []string{"quarantined_until", future}, nil, causeHeld},
		{"suspended at the pass", []string{"state", "suspended"}, nil, causeHeld},
		{"held since", nil, []string{"held_until", future}, causePremise},
		{"quarantined since", nil, []string{"quarantined_until", future}, causePremise},
		{"kill fence since", nil, []string{"state", "asleep", "state_reason", session.KillPendingReason, "sleep_reason", string(session.SleepReasonKilled), "slept_at", gatherNow.Format(time.RFC3339)}, causePremise},
		{"rewoken since", nil, []string{"generation", "4"}, causePremise},
		{"retokened since", nil, []string{"instance_token", "other"}, causePremise},
		{"wake request since", nil, []string{"wake_request", "operator"}, causePremise},
		{"kill fence at the pass", []string{"state_reason", session.KillPendingReason, "sleep_reason", string(session.SleepReasonKilled), "slept_at", gatherNow.Format(time.RFC3339)}, nil, causeHeld},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, _ := launchKit(t, "asleep", c.meta...)
			if c.write != nil {
				k.on(seamAfterReads, func() {})
				k.on(seamAfterReads, func() { k.outside(c.write...) })
			}
			if s := k.runKind(); s.Outcome != settledRefused || s.Cause != c.refuse {
				t.Fatalf("settlement %+v, want refused %s", s, c.refuse)
			}
			if len(k.starts()) != 0 || k.meta("last_woke_at") != "" {
				t.Fatal("a refused PreWake wrote or started")
			}
		})
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

// Kills a resumed start whose immediate death is committed, and a post-call
// read taken before the stale-key wait (START-019): after a start with a
// session_key the launch waits staleKeyDetectDelay on the planner's clock,
// and a runtime dead by then is died during startup, uncommitted.
func TestResumedStartWaitsStaleKeyDetectDelay(t *testing.T) {
	stubTranscript(t, true)
	k, _ := launchKit(t, "asleep", "session_key", "k-1")
	clk := k.p.Clock.(*fakePlannerClock)
	done := make(chan settlement, 1)
	go func() { done <- k.runKind() }()
	clk.waitTimerAt(t, gatherNow.Add(staleKeyDetectDelay))
	k.mutate(func(r *simRuntime) { r.corpse = true })
	k.leaf.mu.Lock()
	reads := len(k.leaf.sinces)
	k.leaf.mu.Unlock()
	clk.Advance(staleKeyDetectDelay)
	s := <-done
	if s.Outcome != settledFailed || s.Cause != causeDiedDuringStartup || k.meta("started_config_hash") != "" {
		t.Fatalf("settlement %+v, want died during startup, uncommitted", s)
	}
	k.leaf.mu.Lock()
	defer k.leaf.mu.Unlock()
	if len(k.leaf.sinces) == reads || !k.leaf.sinces[reads].After(gatherNow) {
		t.Fatalf("fresh reads since %v, want the post-call read after the wait", k.leaf.sinces)
	}
}

// Kills a death after the Start committed or taken as a plain error: a
// runtime gone after a nil Start, and the provider's own died-during-startup
// error, are each died during startup; ErrSessionExists over a dead runtime
// refuses held-dead (C5a3 recycles); an unavailable runtime is deferred, and
// any other error a start error.
func TestLaunchDeathsAndHeldDeadNames(t *testing.T) {
	for _, c := range []struct {
		name  string
		hook  func(k *txKit)
		cause string
	}{
		{"gone after a nil start", func(k *txKit) { k.on(seamAfterCall, func() {}); k.on(seamAfterCall, func() { k.mutate(nil) }) }, causeDiedDuringStartup},
		{"the provider's died error", func(k *txKit) { k.sp.nextStart = runtime.ErrSessionDiedDuringStartup }, causeDiedDuringStartup},
		{"a zombie after a nil start", func(k *txKit) {
			k.on(seamAfterCall, func() {})
			k.on(seamAfterCall, func() { k.mutate(func(r *simRuntime) { r.zombie = true }) })
		}, causeDiedDuringStartup},
		{"runtime unavailable", func(k *txKit) { k.sp.nextStart = runtime.ErrRuntimeUnavailable }, causeStartDeferred},
		{"another start error", func(k *txKit) { k.sp.nextStart = errors.New("boom") }, causeStartError},
		{"a dead held name", func(k *txKit) {
			k.on(seamBeforeCall, func() {})
			k.on(seamBeforeCall, func() { k.runtimeAs("gc-1", "tok", func(r *simRuntime) { r.corpse = true }) })
		}, causeHeldDead},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, _ := launchKit(t, "asleep")
			c.hook(k)
			s := k.runKind()
			if got := s.Cause; got != c.cause || k.meta("started_config_hash") != "" {
				t.Fatalf("settlement %+v, want %s, uncommitted", s, c.cause)
			}
		})
	}
}

// Kills a launch past its deadline, and a provider Start not bounded at
// startup_timeout: the Start's context ends, by deadline, once the clock
// reaches the effect's deadline less its slack; a context that ends during
// the Start, or before it, commits nothing, and before it starts nothing.
func TestLaunchDeadlines(t *testing.T) {
	k, leaf := launchKit(t, "asleep")
	clk := k.p.Clock.(*fakePlannerClock)
	leaf.onStart = func(ctx context.Context) {
		clk.Advance(70*time.Second - startDeadlineSlack)
		select {
		case <-ctx.Done():
			if !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
				t.Errorf("the Start's context ended with %v, want its deadline", context.Cause(ctx))
			}
		case <-time.After(plannerTestGuard):
			t.Error("the Start's context outlived startup_timeout")
		}
	}
	k.runKind()

	k, leaf = launchKit(t, "asleep")
	ctx, cancel := context.WithCancelCause(context.Background())
	leaf.onStart = func(context.Context) { cancel(context.DeadlineExceeded) }
	if s := k.run(ctx, effectSpecs[intentStart]); s.Outcome != settledFailed || s.Cause != causeDeadline || k.meta("started_config_hash") != "" {
		t.Fatalf("ended during the Start: %+v, want a deadline, uncommitted", s)
	}

	k, _ = launchKit(t, "asleep")
	ctx, cancel = context.WithCancelCause(context.Background())
	k.on(seamBeforeCall, func() {})
	k.on(seamBeforeCall, func() { cancel(context.DeadlineExceeded) })
	if s := k.run(ctx, effectSpecs[intentStart]); s.Outcome != settledFailed || len(k.starts()) != 0 {
		t.Fatalf("ended before the Start: %+v after %d starts, want failed with none", s, len(k.starts()))
	}
}

// Kills legacy's blind clear and re-mint (S-1): PreWake fills an empty key
// once and replaces a key whose transcript is gone, in its CAS, and the
// launch runs that key.
func TestStartEffectFreshKeyNotClearedOrReminted(t *testing.T) {
	for _, meta := range [][]string{nil, {"session_key", "stale"}} {
		stubTranscript(t, false)
		k, _ := launchKit(t, "asleep", meta...)
		k.p.Clock.(*fakePlannerClock).auto = true // the stale-key wait
		row := k.p.World.Census.Rows[k.it.Key]
		tp := TemplateParams{TemplateName: "worker", Command: "agent", WorkDir: t.TempDir(), ResolvedProvider: &config.ResolvedProvider{Name: "claude", SessionIDFlag: "--session-id"}}
		k.p.World.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(row.Info): {TP: tp}}}
		var cmd string
		k.p.Runtime.(*startHookLeaf).onStart = func(context.Context) {}
		if s := k.runKind(); s.Outcome != settledLanded {
			t.Fatalf("settlement %+v, want landed", s)
		}
		key := k.meta("session_key")
		if cfgs := k.p.Runtime.(*startHookLeaf).cfgs; len(cfgs) > 0 {
			cmd = cfgs[0].Command
		}
		if key == "" || key == "stale" || !strings.HasSuffix(cmd, "--session-id "+key) {
			t.Fatalf("row key %q, command %q; want a fresh key launched with --session-id", key, cmd)
		}
	}
}

// Kills a Noop that takes a half-open endpoint's single probe, and a launch
// without the ticket (SC N1): only the launch admits, and holds it through
// the Start; its success closes the breaker.
func TestEndpointTicketOnlyOnLaunch(t *testing.T) {
	now := gatherNow
	guard := newEndpointCapacityGuard(func() time.Time { return now })
	k0 := endpointKey("provider:test-agent")
	trip, _ := guard.Admit(k0, "outside", "outside")
	trip.Resolve(verdictCapacity)
	now = now.Add(guard.breaker(k0).Status().BackoffCap)
	probeFree := func() bool {
		ticket, ok := guard.Admit(k0, "probe", "probe")
		ticket.Resolve(verdictNotAttempted)
		return ok
	}
	withGuard := func(k *txKit) {
		row := k.p.World.Census.Rows[k.it.Key]
		tp := TemplateParams{TemplateName: "worker", ResolvedProvider: &config.ResolvedProvider{Name: "test-agent"}}
		k.p.World.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(row.Info): {TP: tp}}}
		k.p.held.start.capacity = guard
		k.it.Endpoint = k0
	}
	noop, _ := launchKit(t, "active")
	withGuard(noop)
	noop.runtimeAs("gc-1", "tok", nil)
	noop.leaf.during = func() {
		if !probeFree() {
			t.Error("a Noop took the endpoint's probe")
		}
	}
	if s := noop.runKind(); s.Outcome != settledNoop {
		t.Fatalf("settlement %+v, want a Noop", s)
	}
	launch, leaf := launchKit(t, "asleep")
	withGuard(launch)
	leaf.onStart = func(context.Context) {
		if probeFree() {
			t.Error("the launch started without the endpoint's probe")
		}
	}
	if s := launch.runKind(); s.Outcome != settledLanded || guard.breaker(k0).Status().State != resilience.StateClosed {
		t.Fatalf("settlement %+v, breaker %v; want landed and the probe's success closing it", s, guard.breaker(k0).Status().State)
	}
}

// Kills a binding dropped from PreWake, and a worktree applied unverified
// (POOL-055, #34): the binding and the verified work dir land in the PreWake
// CAS; refused evidence refuses the start before PreWake with the verdict.
func TestPreWakeFoldsVerifiedBinding(t *testing.T) {
	spec := &worktree.Spec{BeadID: "w-1"}
	for _, verifyErr := range []error{nil, errors.New("not owned")} {
		k, _ := launchKit(t, "asleep")
		k.p.Alloc.Snapshot.Entries[k.it.Key] = &selectionEntry{Binding: &bindingTarget{WorktreeSpec: spec, Patch: session.MetadataPatch{beadmeta.TriggerBeadIDMetadataKey: "w-1"}}}
		old := verifyWorktree
		verifyWorktree = func(worktree.Spec) (worktree.Report, error) { return worktree.Report{Path: "/wt/w-1"}, verifyErr }
		s := k.runKind()
		verifyWorktree = old
		switch {
		case s.Facts.Work == nil || s.Facts.Work.Refused != (verifyErr != nil):
			t.Fatalf("verify %v: work verdict %+v", verifyErr, s.Facts.Work)
		case verifyErr == nil && (k.meta(beadmeta.TriggerBeadIDMetadataKey) != "w-1" || k.meta(beadmeta.WorkDirMetadataKey) != "/wt/w-1"):
			t.Fatalf("the binding and the verified work dir did not land")
		case verifyErr != nil && (s.Cause != causeWorktree || k.meta("generation") != "3"):
			t.Fatalf("refused evidence: settlement %+v; want refused before PreWake", s)
		}
	}
}

// Kills a concrete-pool work_dir repair left to prepare, which fails after
// PreWake on every attempt (the generation climbs): the PreWake CAS carries
// it, and the launch starts in the concrete dir.
func TestPreWakeFoldsConcretePoolRepair(t *testing.T) {
	k, _, concreteDir := concretePoolKit(t, intentStart, "asleep")
	k.it.Deadline = gatherNow.Add(70 * time.Second)
	leaf := &startHookLeaf{recordingLeaf: k.leaf}
	k.p.Runtime = leaf
	if s := k.runKind(); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
	if k.meta(beadmeta.WorkDirMetadataKey) != concreteDir || k.meta("generation") != "4" || len(leaf.cfgs) != 1 || leaf.cfgs[0].WorkDir != concreteDir {
		t.Fatalf("work_dir %q generation %q; want the repair in one PreWake and the concrete dir", k.meta(beadmeta.WorkDirMetadataKey), k.meta("generation"))
	}
}

// Kills a launch blind to the task it was woken for: an agent woken mid-task
// probes its key and starts in the task's checkout from the pass's assigned
// work, unless that read was partial (legacy's resolver).
func TestLaunchUsesAssignedTaskWorkDir(t *testing.T) {
	for _, partial := range []bool{false, true} {
		k, leaf := launchKit(t, "asleep", "session_key", "k-1")
		k.p.Clock.(*fakePlannerClock).auto = true // the stale-key wait
		taskDir, ownDir := t.TempDir(), t.TempDir()
		row := k.p.World.Census.Rows[k.it.Key]
		k.p.World.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(row.Info): {TP: TemplateParams{TemplateName: "worker", WorkDir: ownDir}}}}
		k.p.World.Demand = demandView{StorePartial: partial, AssignedWork: []beads.Bead{{ID: "w-1", Status: "in_progress", Assignee: "gc-1", Metadata: map[string]string{"work_dir": taskDir}}}}
		var probed string
		old := staleResumeKeyProbe
		staleResumeKeyProbe = func(_, dir, _ string) (bool, bool) { probed = dir; return true, true }
		s := k.runKind()
		staleResumeKeyProbe = old
		want := taskDir
		if partial {
			want = ownDir
		}
		if s.Outcome != settledLanded || probed != want || len(leaf.cfgs) != 1 || leaf.cfgs[0].WorkDir != want {
			t.Fatalf("partial %v: settlement %+v, probed %q; want %q", partial, s, probed, want)
		}
	}
}

// Kills a launch under an empty session name (START-012): it refuses at
// PreWake, writing nothing and starting nothing.
func TestLaunchRefusesEmptySessionName(t *testing.T) {
	k, _ := launchKit(t, "asleep", "session_name", "")
	if s := k.runKind(); s.Outcome != settledRefused || s.Cause != causeSessionName || k.meta("generation") != "3" || len(k.starts()) != 0 {
		t.Fatalf("settlement %+v, want refused %s with nothing written", s, causeSessionName)
	}
}

// Kills an ACP launch started before its route exists: the launch routes the
// name to ACP before it starts.
func TestACPLaunchRoutesBeforeStart(t *testing.T) {
	k, leaf := launchKit(t, "asleep")
	row := k.p.World.Census.Rows[k.it.Key]
	k.p.World.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(row.Info): {TP: TemplateParams{TemplateName: "worker", IsACP: true}}}}
	leaf.onStart = func(context.Context) {
		if len(leaf.routed) != 1 || leaf.routed[0] != "s-gc-1" {
			t.Errorf("routes at Start = %v, want s-gc-1 routed to ACP", leaf.routed)
		}
	}
	if s := k.runKind(); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
	tmux, leaf := launchKit(t, "asleep")
	if s := tmux.runKind(); s.Outcome != settledLanded || len(leaf.routed) != 0 {
		t.Fatalf("a non-ACP template: settlement %+v, routes %v; want landed with nothing routed to ACP", s, leaf.routed)
	}
}

// Kills a launch commit that drops legacy's MCP keys, the runtime snapshot
// file, the #46 mirror's clear or the episode's: the commit writes the
// snapshot and identity and zeroes the mirror, the launch rewrites the
// runtime's snapshot file (here, with no servers, removes it), and the Call
// after it clears the accrued episode.
func TestLaunchCommitCarriesMCPAndClearsStartupHealth(t *testing.T) {
	k, _ := launchKit(t, "asleep", session.MCPServersSnapshotMetadataKey, "stale",
		startupHealthActiveCountMetadataKey, "2", startupHealthActiveKindMetadataKey, "crash")
	key := startupHealthEpisodeKey(k.p.World.Census.Rows[k.it.Key].Info, "s-gc-1")
	if err := sessionFrontDoor(k.cache).SaveStartupHealthEpisode(session.StartupHealthEpisode{SessionName: key, ConsecutiveCount: 2}); err != nil {
		t.Fatal(err)
	}
	row := k.p.World.Census.Rows[k.it.Key]
	tp := TemplateParams{TemplateName: "worker", Env: map[string]string{"GC_CITY_PATH": k.p.World.CityPath}}
	k.p.World.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(row.Info): {TP: tp}}}
	snapshot := citylayout.RuntimePath(k.p.World.CityPath, "session-mcp", "gc-1.json")
	if err := os.MkdirAll(filepath.Dir(snapshot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshot, []byte(`[{"name":"stale"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := k.runKind(); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
	if k.meta(session.MCPServersSnapshotMetadataKey) == "stale" || k.meta(session.MCPIdentityMetadataKey) == "" ||
		k.meta(startupHealthActiveCountMetadataKey) != "0" || k.meta(startupHealthActiveKindMetadataKey) != "" {
		t.Fatalf("row after the commit: snapshot %q identity %q mirror %q/%q", k.meta(session.MCPServersSnapshotMetadataKey),
			k.meta(session.MCPIdentityMetadataKey), k.meta(startupHealthActiveCountMetadataKey), k.meta(startupHealthActiveKindMetadataKey))
	}
	if ep, err := sessionFrontDoor(k.cache).LoadStartupHealthEpisode(key); err != nil || ep.ConsecutiveCount != 0 {
		t.Fatalf("episode %+v (%v), want cleared", ep, err)
	}
	if _, err := os.Stat(snapshot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the runtime MCP snapshot file survived a launch with no MCP servers (%v)", err)
	}
}

// Kills S2's premise read as the default rule after the Start: a row the CLI
// confirmed active with the same token still commits, and `gc session
// kill`'s fence (asleep, the same token) landed during the Start refuses
// with the premise, committing nothing.
func TestLaunchCommitPremise(t *testing.T) {
	for _, c := range []struct {
		name   string
		write  []string
		landed bool
	}{
		{"CLI confirmed active", []string{"state", "active", "state_reason", "creation_complete"}, true},
		{"kill fence", []string{"state", "asleep", "sleep_reason", "killed"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, leaf := launchKit(t, "asleep")
			leaf.onStart = func(context.Context) { k.outside(c.write...) }
			s := k.runKind()
			if got := k.meta("started_config_hash") != ""; got != c.landed || (!got && s.Cause != causePremise) {
				t.Fatalf("settlement %+v: committed=%v, want %v (else the premise)", s, got, c.landed)
			}
		})
	}
}

// Kills an endpoint-gate refusal or a provider capacity failure backed off
// as a row failure: each is a deferral the breaker paces, as a swap pause
// is (legacy keeps the row's place).
func TestEndpointGateRefusalDoesNotBackOffRow(t *testing.T) {
	for _, s := range []settlement{
		{Outcome: settledRefused, Cause: causeEndpointGate},
		{Outcome: settledFailed, Cause: causeCapacity},
	} {
		p := newPlanner(newFakePlannerClock(plannerT0), func() time.Duration { return time.Minute }, nil, newInflightMap(), nil, nil)
		k := rowKey{Leg: rowLeg, ID: "a"}
		s.Key, s.Kind, s.At = k, intentStart, plannerT0
		p.backoffSettled(s)
		if r, ok := p.backoff.Snapshot()[rowBackoffKey(k)]; ok {
			t.Fatalf("row backoff %+v after %s, want none", r, s.Cause)
		}
	}
}

// Kills a PreWake that trusts the verb section's read: a runtime that
// appears under the name after the ticket is admitted refuses
// runtime-present, writing no PreWake and starting nothing.
func TestPreWakeRefusesARuntimeThatAppeared(t *testing.T) {
	k, _ := launchKit(t, "asleep")
	k.on(seamAfterCall, func() { k.runtimeAs("gc-2", "theirs", nil) })
	if s := k.runKind(); s.Outcome != settledRefused || s.Cause != causeRuntimePresent {
		t.Fatalf("settlement %+v, want refused %s", s, causeRuntimePresent)
	}
	if len(k.starts()) != 0 || k.meta("generation") != "3" {
		t.Fatal("a refused PreWake wrote or started")
	}
}

// Kills a ticket resolved with the wrong verdict: from a half-open breaker,
// a landed launch closes it, a provider capacity error reopens it, and a
// start that died before its commit leaves it half-open.
func TestEndpointTicketVerdicts(t *testing.T) {
	for _, c := range []struct {
		name  string
		hook  func(k *txKit)
		state resilience.State
		trips int
	}{
		{"landed", func(*txKit) {}, resilience.StateClosed, 1},
		{"capacity", func(k *txKit) { k.sp.nextStart = fmt.Errorf("quota: %w", runtime.ErrProviderCapacity) }, resilience.StateOpen, 2},
		{"died before the commit", func(k *txKit) { k.on(seamAfterCall, func() {}); k.on(seamAfterCall, func() { k.mutate(nil) }) }, resilience.StateHalfOpen, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			now := gatherNow
			guard := newEndpointCapacityGuard(func() time.Time { return now })
			k0 := endpointKey("provider:test-agent")
			trip, _ := guard.Admit(k0, "outside", "outside")
			trip.Resolve(verdictCapacity)
			now = now.Add(guard.breaker(k0).Status().BackoffCap)
			k, _ := launchKit(t, "asleep")
			row := k.p.World.Census.Rows[k.it.Key]
			tp := TemplateParams{TemplateName: "worker", ResolvedProvider: &config.ResolvedProvider{Name: "test-agent"}}
			k.p.World.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(row.Info): {TP: tp}}}
			k.p.held.start.capacity = guard
			k.it.Endpoint = k0
			c.hook(k)
			s := k.runKind()
			if st := guard.breaker(k0).Status(); st.State != c.state || st.Trips != c.trips {
				t.Fatalf("settlement %+v, breaker %v trips %d; want %v trips %d", s, st.State, st.Trips, c.state, c.trips)
			}
		})
	}
}

// Kills a launch that starts an agent without its hooks and ACP route, that
// installs them before PreWake lands or after the Start, and an install on
// any verb but Launch: the row's resolved agent's side effects run once,
// after PreWake landed and before the provider Start, and a no-op, an adopt
// and a refused PreWake install nothing.
func TestLaunchInstallsAgentSideEffectsBeforeStart(t *testing.T) {
	agent := &config.Agent{Name: "worker"}
	for _, c := range []struct {
		name  string
		kind  string
		state string
		setup func(*txKit)
		want  []string
	}{
		{"launch", intentStart, "asleep", nil, []string{"install@generation=4", "start"}},
		{"no-op over a committed runtime", intentStart, "active", func(k *txKit) { k.runtimeAs("gc-1", "tok", nil) }, nil},
		{"adopt", intentAdopt, "creating", func(k *txKit) { k.runtimeAs("gc-1", "tok", nil) }, nil},
		{"PreWake refused", intentStart, "asleep", func(k *txKit) {
			k.on(seamAfterReads, func() {})
			k.on(seamAfterReads, func() { k.outside("held_until", gatherNow.Add(time.Hour).Format(time.RFC3339)) })
		}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, leaf := launchKit(t, c.state)
			k.it.Kind = c.kind
			var calls []string
			row := k.p.World.Census.Rows[k.it.Key].Info
			tp, _ := k.p.World.Templates.lookup(row)
			k.p.World.Templates = &templateMemo{
				entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(row): {TP: tp.TP, Agent: agent}},
				install: func(a *config.Agent, got TemplateParams) {
					if a != agent || got.TemplateName != "worker" {
						t.Errorf("installed for %v %q, want the row's agent and template", a, got.TemplateName)
					}
					calls = append(calls, "install@generation="+k.meta("generation"))
				},
			}
			leaf.onStart = func(context.Context) { calls = append(calls, "start") }
			if c.setup != nil {
				c.setup(k)
			}
			k.runKind()
			if !reflect.DeepEqual(calls, c.want) {
				t.Fatalf("calls %v, want %v", calls, c.want)
			}
		})
	}
}

// Kills prepare run without the PreWake's transcript state (S-1): a key
// whose transcript the PreWake found present resumes the conversation.
func TestLaunchResumesAPresentTranscript(t *testing.T) {
	stubTranscript(t, true)
	k, leaf := launchKit(t, "asleep", "session_key", "k-1")
	k.p.Clock.(*fakePlannerClock).auto = true // the stale-key wait
	row := k.p.World.Census.Rows[k.it.Key]
	tp := TemplateParams{
		TemplateName: "worker", Command: "agent", WorkDir: t.TempDir(),
		ResolvedProvider: &config.ResolvedProvider{Name: "claude", SessionIDFlag: "--session-id", ResumeFlag: "--resume", ResumeStyle: "flag"},
	}
	k.p.World.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(row.Info): {TP: tp}}}
	if s := k.runKind(); s.Outcome != settledLanded || len(leaf.cfgs) != 1 || !strings.Contains(leaf.cfgs[0].Command, "--resume k-1") {
		t.Fatalf("settlement %+v, configs %+v; want one launch resuming k-1", s, leaf.cfgs)
	}
}

// Kills a launch through an open endpoint breaker (SC N1): before its
// backoff ends, the launch refuses endpoint-gate and starts nothing.
func TestLaunchRefusedByAnOpenEndpoint(t *testing.T) {
	guard := newEndpointCapacityGuard(func() time.Time { return gatherNow })
	k0 := endpointKey("provider:test-agent")
	trip, _ := guard.Admit(k0, "outside", "outside")
	trip.Resolve(verdictCapacity)
	k, _ := launchKit(t, "asleep")
	row := k.p.World.Census.Rows[k.it.Key]
	tp := TemplateParams{TemplateName: "worker", ResolvedProvider: &config.ResolvedProvider{Name: "test-agent"}}
	k.p.World.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(row.Info): {TP: tp}}}
	k.p.held.start.capacity = guard
	k.it.Endpoint = k0
	if s := k.runKind(); s.Outcome != settledRefused || s.Cause != causeEndpointGate || len(k.starts()) != 0 || k.meta("generation") != "3" {
		t.Fatalf("settlement %+v, starts %d; want refused endpoint-gate, nothing written or started", s, len(k.starts()))
	}
}

// Kills a PreWake that refuses a wait-ready wake (POOL-077/080: the
// allocation wakes a wait-held row whose wait is ready; its hold clears only
// once the nudge reaches the running session) or wedges on an unparseable
// timer no heal clears: each launches.
func TestPreWakeLaunchesWaitHeldAndUnparseableTimerRows(t *testing.T) {
	for _, meta := range [][]string{
		{"wait_hold", "true", "sleep_intent", "wait-hold", "sleep_reason", "wait-hold"},
		{"held_until", "not-a-time"},
		{"quarantined_until", "not-a-time"},
	} {
		k, _ := launchKit(t, "asleep", meta...)
		if s := k.runKind(); s.Outcome != settledLanded || len(k.starts()) != 1 {
			t.Errorf("%v: settlement %+v, starts %d; want one launch", meta, s, len(k.starts()))
		}
	}
}

// Kills a PreWake that trusts the verb section's read of what is under the
// name: a corpse, a zombie or an unreadable runtime that appears after the
// ticket is admitted refuses runtime-present, writing nothing.
func TestPreWakeRefusesWhateverAppearedUnderTheName(t *testing.T) {
	for _, c := range []struct {
		name string
		mark func(*simRuntime)
	}{
		{"a corpse", func(r *simRuntime) { r.corpse = true }},
		{"a zombie", func(r *simRuntime) { r.zombie = true }},
		{"an unreadable runtime", func(r *simRuntime) { r.probeErr = true }},
	} {
		k, _ := launchKit(t, "asleep")
		k.on(seamAfterCall, func() { k.runtimeAs("gc-1", "tok", c.mark) })
		if s := k.runKind(); s.Outcome != settledRefused || s.Cause != causeRuntimePresent || len(k.starts()) != 0 || k.meta("generation") != "3" {
			t.Errorf("%s: settlement %+v, starts %d; want refused %s, nothing written or started", c.name, s, len(k.starts()), causeRuntimePresent)
		}
	}
}

// Kills a commit on an identity read off a runtime object that changed
// across it (I3, O2): the commit refuses liveness-unknown and commits
// nothing.
func TestLaunchCommitNeedsTheSameObject(t *testing.T) {
	k, leaf := launchKit(t, "asleep")
	leaf.onStart = func(context.Context) {
		reads := 0
		k.leaf.during = func() {
			if reads++; reads == 2 { // between the bracketing liveness reads
				token := k.meta("instance_token")
				k.sp.mu.Lock()
				k.sp.put("s-gc-1", "gc-1", "4", token)
				k.sp.mu.Unlock()
			}
		}
	}
	if s := k.runKind(); s.Outcome != settledRefused || s.Cause != causeLivenessUnknown || k.meta("started_config_hash") != "" {
		t.Fatalf("settlement %+v, want refused %s and nothing committed", s, causeLivenessUnknown)
	}
}

// Kills a commit that a capacity or deferred error vetoes over a runtime
// that is already up and Current: the commit decides on the runtime first.
func TestLaunchCommitsAnAliveRuntimeWhateverTheStartReturned(t *testing.T) {
	for _, startErr := range []error{fmt.Errorf("quota: %w", runtime.ErrProviderCapacity), runtime.ErrSessionInitializing} {
		k, leaf := launchKit(t, "asleep")
		k.sp.nextStart = startErr
		leaf.onStart = func(context.Context) {
			token := k.meta("instance_token")
			k.sp.mu.Lock()
			k.sp.put("s-gc-1", "gc-1", "4", token)
			k.sp.mu.Unlock()
		}
		if s := k.runKind(); s.Outcome != settledLanded || k.meta("started_config_hash") == "" {
			t.Errorf("%v: settlement %+v, want the alive Current runtime committed", startErr, s)
		}
	}
}

// Kills a stale-key wait deaf to the effect's context (START-019): a context
// that ends during the wait ends the effect without the clock moving.
func TestStaleKeyWaitEndsWithTheContext(t *testing.T) {
	stubTranscript(t, true)
	k, _ := launchKit(t, "asleep", "session_key", "k-1")
	clk := k.p.Clock.(*fakePlannerClock)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan settlement, 1)
	go func() { done <- k.run(ctx, effectSpecs[intentStart]) }()
	clk.waitTimerAt(t, gatherNow.Add(staleKeyDetectDelay))
	cancel()
	select {
	case s := <-done:
		if s.Outcome == settledLanded {
			t.Fatalf("settlement %+v, want the effect ended by its context", s)
		}
	case <-time.After(plannerTestGuard):
		t.Fatal("the stale-key wait outlived its context")
	}
}

// Kills a landed commit settled as failed when the context ends before the
// after-commit Call (it never fails the effect): the settlement stays landed
// and the ticket is credited a success.
func TestLaunchCommitLandedThenContextEnds(t *testing.T) {
	now := gatherNow
	guard := newEndpointCapacityGuard(func() time.Time { return now })
	k0 := endpointKey("provider:test-agent")
	trip, _ := guard.Admit(k0, "outside", "outside")
	trip.Resolve(verdictCapacity)
	now = now.Add(guard.breaker(k0).Status().BackoffCap)
	k, _ := launchKit(t, "asleep")
	k.p.held.start.capacity, k.it.Endpoint = guard, k0
	ctx, cancel := context.WithCancelCause(context.Background())
	k.on(seamAfterWrite, func() {})                                   // PreWake's write
	k.on(seamAfterWrite, func() { cancel(context.DeadlineExceeded) }) // the commit's write
	s := k.run(ctx, effectSpecs[intentStart])
	if s.Outcome != settledLanded || k.meta("state") != "active" || guard.breaker(k0).Status().State != resilience.StateClosed {
		t.Fatalf("settlement %+v, state %q, breaker %v; want landed, active, closed", s, k.meta("state"), guard.breaker(k0).Status().State)
	}
}

// Kills a runtime MCP snapshot written after a failed Start (legacy persists
// it on success only): a failed Start leaves the prior snapshot, and a
// capacity refusal settles capacity even when the snapshot could not be
// cleared.
func TestLaunchPersistsTheMCPSnapshotOnSuccessOnly(t *testing.T) {
	for _, c := range []struct {
		name     string
		startErr error
		cause    string
		blocked  bool // the snapshot path is a non-empty dir: a clear would fail
	}{
		{"a failed start", errors.New("boom"), causeStartError, false},
		{"a capacity refusal", fmt.Errorf("quota: %w", runtime.ErrProviderCapacity), causeCapacity, true},
	} {
		k, _ := launchKit(t, "asleep")
		row := k.p.World.Census.Rows[k.it.Key]
		tp := TemplateParams{TemplateName: "worker", Env: map[string]string{"GC_CITY_PATH": k.p.World.CityPath}}
		k.p.World.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(row.Info): {TP: tp}}}
		snapshot := citylayout.RuntimePath(k.p.World.CityPath, "session-mcp", "gc-1.json")
		if c.blocked {
			if err := os.MkdirAll(filepath.Join(snapshot, "x"), 0o700); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(snapshot), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(snapshot, []byte(`[{"name":"prior"}]`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		k.sp.nextStart = c.startErr
		s := k.runKind()
		if _, err := os.Stat(snapshot); s.Cause != c.cause || err != nil {
			t.Errorf("%s: settlement %+v, snapshot %v; want %s and the prior snapshot kept", c.name, s, err, c.cause)
		}
	}
}

// Kills a stale resume key cleared without rotating the continuation
// (legacy's commitPendingContinuationReset at the start, chat.go): the
// PreWake bumps continuation_epoch and clears the pending mark in its own
// write, and the runtime starts under the new epoch.
func TestStaleKeyResetRotatesTheContinuation(t *testing.T) {
	stubTranscript(t, false)
	k, leaf := launchKit(t, "asleep", "session_key", "stale", "continuation_epoch", "5", "started_config_hash", "h0")
	k.p.Clock.(*fakePlannerClock).auto = true
	row := k.p.World.Census.Rows[k.it.Key]
	tp := TemplateParams{TemplateName: "worker", Command: "agent", WorkDir: t.TempDir(), ResolvedProvider: &config.ResolvedProvider{Name: "claude", SessionIDFlag: "--session-id"}}
	k.p.World.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(row.Info): {TP: tp}}}
	s := k.runKind()
	if s.Outcome != settledLanded || len(leaf.cfgs) != 1 {
		t.Fatalf("settlement %+v, want one launch", s)
	}
	if env := leaf.cfgs[0].Env["GC_CONTINUATION_EPOCH"]; env != "6" || k.meta("continuation_epoch") != "6" || k.meta("continuation_reset_pending") != "" {
		t.Fatalf("runtime epoch %q, row epoch %q, reset pending %q; want 6, 6 and cleared", env, k.meta("continuation_epoch"), k.meta("continuation_reset_pending"))
	}
}

// Kills later sections keyed on the bound row, which a PreWake binding takes
// out of the pass's template memo: the commit's read still carries the
// template's process names, session.woke its subject, and a name taken
// during the Start still refuses occupied.
func TestLaunchKeysOnTheVerbSectionsTemplate(t *testing.T) {
	k, _ := launchKit(t, "asleep")
	k.p.Alloc.Snapshot.Entries[k.it.Key] = &selectionEntry{Binding: &bindingTarget{Patch: session.MetadataPatch{"gc.trigger_bead_id": "w-1"}}}
	s := k.runKind()
	k.leaf.mu.Lock()
	names := slices.Clone(k.leaf.names)
	k.leaf.mu.Unlock()
	if s.Outcome != settledLanded || len(s.Facts.Events) != 1 || s.Facts.Events[0].Subject != "worker" || len(names[len(names)-1]) == 0 {
		t.Fatalf("settlement %+v, process names per read %v; want landed, session.woke for worker, the commit read with process names", s, names)
	}
	k, leaf := launchKit(t, "asleep")
	k.p.Alloc.Snapshot.Entries[k.it.Key] = &selectionEntry{Binding: &bindingTarget{Patch: session.MetadataPatch{"gc.trigger_bead_id": "w-1"}}}
	leaf.onStart = func(context.Context) { k.runtimeAs("gc-2", "theirs", nil) }
	if s := k.runKind(); s.Cause != causeOccupied {
		t.Fatalf("a name taken during the Start: cause %q, want %s", s.Cause, causeOccupied)
	}
}

// Kills a launch env carrying the trigger the template was resolved under
// after PreWake cleared or re-pointed it: the trigger env and the demand
// origin follow the bound row (legacy binds, then resolves).
func TestLaunchTriggerEnvFollowsTheBoundRow(t *testing.T) {
	stale := map[string]string{"GC_TRIGGER_BEAD_ID": "w-old", "GC_TRIGGER_WORK_BEAD_ID": "w-old", "GC_SPAWN_ORIGIN": "demand", "GC_KEEP": "1"}
	for _, c := range []struct {
		name, bind string
		want       map[string]string
	}{
		{"cleared", "", map[string]string{"GC_TRIGGER_BEAD_ID": "", "GC_SPAWN_ORIGIN": "", "GC_KEEP": "1"}},
		{"re-pointed", "w-new", map[string]string{"GC_TRIGGER_BEAD_ID": "w-new", "GC_TRIGGER_WORK_BEAD_ID": "w-new", "GC_SPAWN_ORIGIN": "demand", "GC_KEEP": "1"}},
	} {
		k, leaf := launchKit(t, "asleep", "gc.trigger_bead_id", "w-old")
		row := k.p.World.Census.Rows[k.it.Key]
		k.p.World.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(row.Info): {TP: TemplateParams{TemplateName: "worker", Env: maps.Clone(stale)}}}}
		k.p.Alloc.Snapshot.Entries[k.it.Key] = &selectionEntry{Binding: &bindingTarget{Patch: session.MetadataPatch{"gc.trigger_bead_id": c.bind}}}
		if s := k.runKind(); s.Outcome != settledLanded || len(leaf.cfgs) != 1 {
			t.Fatalf("%s: settlement %+v, want one launch", c.name, s)
		}
		for key, want := range c.want {
			if got := leaf.cfgs[0].Env[key]; got != want {
				t.Errorf("%s: launch %s = %q, want %q", c.name, key, got, want)
			}
		}
	}
}

// Kills breaker transitions left unflushed and a planner that drops the
// controller's guard on the way to its effects: the host's guard reaches the
// pass's start caps, and a ticket's resolution flushes its transition to the
// recorder.
func TestPlannerHandsItsGuardToTheStartAndFlushes(t *testing.T) {
	now := gatherNow
	guard := newEndpointCapacityGuard(func() time.Time { return now })
	rec := events.NewFake()
	k, _ := launchKit(t, "asleep")
	k0 := endpointKey("provider:test-agent")
	trip, _ := guard.Admit(k0, "outside", "outside")
	trip.Resolve(verdictCapacity)
	guard.flush(nil, nil)
	now = now.Add(guard.breaker(k0).Status().BackoffCap)
	k.p.held.start.capacity, k.p.held.start.rec, k.it.Endpoint = guard, rec, k0
	if s := k.runKind(); s.Outcome != settledLanded || len(rec.Events) == 0 {
		t.Fatalf("settlement %+v, events %v: the ticket's breaker transition was not flushed to the recorder", s, rec.Events)
	}

	rt := newDefaultPlanner(io.Discard)
	rt.bindHost(plannerHost{gather: gatherEnv{CityPath: t.TempDir(), Capacity: func() *endpointCapacityGuard { return guard }}})
	if rt.planner.capacity == nil || rt.planner.capacity() != guard {
		t.Fatal("bindHost dropped the host's capacity guard")
	}
	var handed *endpointCapacityGuard
	withSpecs(t, func(specs map[string]effectSpec) {
		specs[intentStart] = effectSpec{class: capStarts, caps: capProviderStart, body: func(_ context.Context, c txCaps) settlement {
			handed = c.start.capacity
			return settlement{Outcome: settledNoop}
		}}
	})
	x := newEffectExecutor(func(settlement) {}, io.Discard)
	x.spawn = func(f func()) { f() }
	rt.planner.effects = x
	w := &World{Now: gatherNow, Census: readCensus(t, gatherNow, censusLegs(rowLeg, censusStore()))}
	rt.planner.submit(w, &allocDecision{}, []intent{{Kind: intentStart, Key: rowKeyOf("gc-1")}})
	if handed != guard {
		t.Fatalf("the pass's start caps carry guard %p, want the host's %p", handed, guard)
	}
}
