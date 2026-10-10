package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

// stubTranscript makes every keyed transcript present or absent.
func stubTranscript(t *testing.T, present bool) {
	t.Helper()
	old := staleResumeKeyProbe
	staleResumeKeyProbe = func(string, string, string) (bool, bool) { return present, true }
	t.Cleanup(func() { staleResumeKeyProbe = old })
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

// Kills an endpoint-gate refusal backed off as a row failure: it is a
// deferral the breaker paces, as a swap pause is.
func TestEndpointGateRefusalDoesNotBackOffRow(t *testing.T) {
	p := newPlanner(newFakePlannerClock(plannerT0), func() time.Duration { return time.Minute }, nil, newInflightMap(), nil, nil)
	k := rowKey{Leg: rowLeg, ID: "a"}
	p.backoffSettled(settlement{Key: k, Kind: intentStart, Outcome: settledRefused, Cause: causeEndpointGate, At: plannerT0})
	if r, ok := p.backoff.Snapshot()[rowBackoffKey(k)]; ok {
		t.Fatalf("row backoff %+v after an endpoint-gate refusal, want none", r)
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
			c.hook(k)
			s := k.runKind()
			if st := guard.breaker(k0).Status(); st.State != c.state || st.Trips != c.trips {
				t.Fatalf("settlement %+v, breaker %v trips %d; want %v trips %d", s, st.State, st.Trips, c.state, c.trips)
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
	if s := k.runKind(); s.Outcome != settledRefused || s.Cause != causeEndpointGate || len(k.starts()) != 0 || k.meta("generation") != "3" {
		t.Fatalf("settlement %+v, starts %d; want refused endpoint-gate, nothing written or started", s, len(k.starts()))
	}
}
