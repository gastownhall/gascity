package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// inc003Env builds a live, active session bead that is not in the desired
// state. With configured=false the template is absent from config and the drain
// reason is "orphaned"; with configured=true the template is configured but
// undesired and the reason is "suspended". wokeAt returns the raw last_woke_at
// for the env's clock.
func inc003Env(t *testing.T, configured bool, wokeAt func(now time.Time) string) (*reconcilerTestEnv, beads.Bead) {
	t.Helper()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	if configured {
		env.cfg = &config.City{
			Agents:        []config.Agent{{Name: "worker"}},
			NamedSessions: []config.NamedSession{{Template: "worker"}},
		}
	}
	name := "orphan"
	if configured {
		name = "worker"
	}
	if err := env.sp.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	session := env.createSessionBead(name, name)
	env.setSessionMetadata(&session, map[string]string{"state": "active", "last_woke_at": wokeAt(env.clk.Now())})
	return env, session
}

func wokeAgo(d time.Duration) func(now time.Time) string {
	return func(now time.Time) string { return now.Add(-d).UTC().Format(time.RFC3339) }
}

// reconcileINC003Traced runs one traced reconcile tick for cityPath, so a test
// can exercise the runtime suspension state `gc suspend` writes under it.
func (e *reconcilerTestEnv) reconcileINC003Traced(cityPath string, trace *sessionReconcilerTraceCycle, sessions ...beads.Bead) {
	cfgNames := configuredSessionNames(e.cfg, "", e.store)
	reconcileSessionBeadsTraced(
		context.Background(), cityPath, sessions, e.desiredState, cfgNames, e.cfg, e.sp,
		e.store, nil, nil, nil, nil, e.dt, map[string]int{}, false, nil, "",
		nil, e.clk, e.rec, 0, 0, &e.stdout, &e.stderr, trace,
		e.startOptions...,
	)
}

func inc003GraceRecord(t *testing.T, trace *sessionReconcilerTraceCycle, name string) SessionReconcilerTraceRecord {
	t.Helper()
	for _, r := range trace.records {
		if r.SiteCode == TraceSiteReconcilerOrphaned && r.SessionName == name && r.ReasonCode == TraceReasonUndesiredWakeGrace {
			return r
		}
	}
	t.Fatalf("no %s/%s trace record for %q; records: %+v", TraceSiteReconcilerOrphaned, TraceReasonUndesiredWakeGrace, name, trace.records)
	return SessionReconcilerTraceRecord{}
}

// A live undesired session woken 30s ago is kept: no drain begins, the runtime
// stays up, and the deferral is traced and logged. Without the grace this is
// the INC-003 storm (drain the fresh seat, ack, kill, re-wake; ~34 kills/min).
func TestReconcileSessionBeads_INC003_UndesiredWokeUnder5mKept(t *testing.T) {
	env, session := inc003Env(t, false, wokeAgo(30*time.Second))
	wokeAt := session.Metadata["last_woke_at"]
	trace := newPoolDesiredStateTestTrace("orphan")

	env.reconcileINC003Traced("", trace, session)

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("drain began inside the undesired-wake grace: reason=%q", ds.reason)
	}
	if !env.sp.IsRunning("orphan") {
		t.Fatal("runtime stopped inside the undesired-wake grace")
	}
	r := inc003GraceRecord(t, trace, "orphan")
	if r.OutcomeCode != TraceOutcomeDeferred {
		t.Errorf("outcome = %q, want %q", r.OutcomeCode, TraceOutcomeDeferred)
	}
	for key, want := range map[string]any{
		"drain_reason":   "orphaned",
		"last_woke_at":   wokeAt,
		"grace_s":        300,
		"provider_alive": true,
	} {
		if got := r.Fields[key]; got != want {
			t.Errorf("trace field %s = %#v, want %#v", key, got, want)
		}
	}
	if !strings.Contains(env.stdout.String(), "Skipping orphaned drain for 'orphan': woken within the 5m0s undesired-wake grace") {
		t.Errorf("stdout missing grace skip line:\n%s", env.stdout.String())
	}
}

// The grace is level-triggered and bounded: the same row is kept one second
// before the grace ends and drained on the first tick at the boundary.
func TestReconcileSessionBeads_INC003_DrainsAfterGrace(t *testing.T) {
	env, session := inc003Env(t, false, wokeAgo(0))
	woke := env.clk.Now()

	env.clk.Time = woke.Add(wakeUndesiredGrace - time.Second)
	env.reconcile([]beads.Bead{session})
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("drain began 1s before the grace ended: reason=%q", ds.reason)
	}

	env.clk.Time = woke.Add(wakeUndesiredGrace)
	env.reconcile([]beads.Bead{session})
	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatal("expected the orphaned drain once the grace elapsed")
	}
	if ds.reason != "orphaned" {
		t.Errorf("drain reason = %q, want %q", ds.reason, "orphaned")
	}
}

// An empty or unparseable last_woke_at is no evidence of a recent wake, so the
// row drains on the first tick as it did before INC-003.
func TestReconcileSessionBeads_INC003_UnparseableLastWokeDrains(t *testing.T) {
	for _, tc := range []struct{ name, lastWokeAt string }{
		{"empty", ""},
		{"garbage", "not-a-time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, session := inc003Env(t, false, func(time.Time) string { return tc.lastWokeAt })

			env.reconcile([]beads.Bead{session})

			ds := env.dt.get(session.ID)
			if ds == nil {
				t.Fatalf("last_woke_at=%q must not earn the grace; expected an orphaned drain", tc.lastWokeAt)
			}
			if ds.reason != "orphaned" {
				t.Errorf("drain reason = %q, want %q", ds.reason, "orphaned")
			}
		})
	}
}

// CONTRACT v4 §3.3 arm 18 / C4.9 covers every undesired drain, not only
// orphans: a configured-but-undesired ("suspended") row woken inside the grace
// is kept too.
func TestReconcileSessionBeads_INC003_SuspendedReasonAlsoDeferred(t *testing.T) {
	env, session := inc003Env(t, true, wokeAgo(time.Minute))
	trace := newPoolDesiredStateTestTrace("worker")

	env.reconcileINC003Traced("", trace, session)

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("suspended drain began inside the undesired-wake grace: reason=%q", ds.reason)
	}
	if got := inc003GraceRecord(t, trace, "worker").Fields["drain_reason"]; got != "suspended" {
		t.Errorf("trace drain_reason = %#v, want %q", got, "suspended")
	}
}

// inc003DrainTraced asserts the row drained for reason with no grace traced,
// and that the drain trace names the operator suspend cause.
func inc003DrainTraced(t *testing.T, env *reconcilerTestEnv, trace *sessionReconcilerTraceCycle, session beads.Bead, name, reason, cause string) {
	t.Helper()
	if ds := env.dt.get(session.ID); ds == nil || ds.reason != reason {
		t.Fatalf("drain = %+v, want a %s drain on this tick (cause %s)", ds, reason, cause)
	}
	var drained bool
	for _, r := range trace.records {
		if r.ReasonCode == TraceReasonUndesiredWakeGrace {
			t.Fatalf("grace traced under a %s suspend: %+v", cause, r)
		}
		if r.SiteCode == TraceSiteReconcilerOrphaned && r.SessionName == name && r.OutcomeCode == TraceOutcomeDrain {
			drained = true
			if got := r.Fields["suspend_cause"]; got != cause {
				t.Errorf("drain trace suspend_cause = %#v, want %q", got, cause)
			}
		}
	}
	if !drained {
		t.Fatalf("no drain trace record for %q; records: %+v", name, trace.records)
	}
}

// inc003LiveRow starts a live, active row woken a minute ago with extra
// metadata, and returns its bead.
func inc003LiveRow(t *testing.T, env *reconcilerTestEnv, name, template string, extra map[string]string) beads.Bead {
	t.Helper()
	if err := env.sp.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	session := env.createSessionBead(name, template)
	meta := map[string]string{"state": "active", "last_woke_at": wokeAgo(time.Minute)(env.clk.Now())}
	for k, v := range extra {
		meta[k] = v
	}
	env.setSessionMetadata(&session, meta)
	return session
}

// suspendCityAt and suspendRigAt write the runtime suspension state exactly as
// `gc suspend` and `gc rig suspend` do.
func suspendCityAt(t *testing.T, cityPath string) {
	t.Helper()
	on := true
	if err := suspensionstate.SetCitySuspended(fsys.OSFS{}, cityPath, &on); err != nil {
		t.Fatal(err)
	}
}

func suspendRigAt(t *testing.T, cityPath, rig string) {
	t.Helper()
	on := true
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityPath, rig, &on); err != nil {
		t.Fatal(err)
	}
}

// inc003SuspendEnv builds a live, active, undesired session bead woken a minute
// ago whose agent is configured, then applies an operator suspend at scope
// ("none", "city", "rig", "agent" or "session"). With configured=false the
// agent has no named session and the drain reason is "orphaned"; with
// configured=true it has one and the reason is "suspended". The rig scope puts
// the agent in rig "myrig"; the session scope writes `gc session suspend`'s
// managed hold.
func inc003SuspendEnv(t *testing.T, configured bool, scope string) (*reconcilerTestEnv, beads.Bead, string, string) {
	t.Helper()
	env := newReconcilerTestEnv()
	agentCfg := config.Agent{Name: "orphan"}
	if configured {
		agentCfg.Name = "worker"
	}
	env.cfg = &config.City{}
	var extra map[string]string
	switch scope {
	case "none":
	case "city":
		env.cfg.Workspace.SuspendedOnStart = true
	case "rig":
		agentCfg.Dir = "myrig"
		env.cfg.Rigs = []config.Rig{{Name: "myrig", Suspended: true}}
	case "agent":
		agentCfg.Suspended = true
	case "session":
		extra = map[string]string{
			"held_until":   env.clk.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
			"sleep_intent": "user-hold",
			"state":        "suspended",
		}
	default:
		t.Fatalf("unknown suspend scope %q", scope)
	}
	env.cfg.Agents = []config.Agent{agentCfg}
	identity := agentCfg.QualifiedName()
	name := identity
	if configured {
		env.cfg.NamedSessions = []config.NamedSession{{Template: agentCfg.Name, Dir: agentCfg.Dir}}
		name = config.NamedSessionRuntimeName("", env.cfg.Workspace, identity)
	}
	return env, inc003LiveRow(t, env, name, identity, extra), name, identity
}

// An operator suspend (city, rig, agent or session) is explicit intent, not a
// lagging desired-state view, so it gets no grace: both undesired reasons
// drain on the first tick although the row was woken a minute ago (suspension
// is quiescence, #7115), and the drain trace names the cause. With no suspend
// the same configured rows keep the grace.
func TestReconcileSessionBeads_INC003_OperatorSuspendDrainsWithoutGrace(t *testing.T) {
	for _, scope := range []string{"none", "city", "rig", "agent", "session"} {
		for _, tc := range []struct {
			configured bool
			reason     string
		}{
			{false, "orphaned"},
			{true, "suspended"},
		} {
			t.Run(scope+"/"+tc.reason, func(t *testing.T) {
				env, session, name, template := inc003SuspendEnv(t, tc.configured, scope)
				trace := newPoolDesiredStateTestTrace(name, template)

				env.reconcileINC003Traced("", trace, session)

				if scope == "none" {
					if ds := env.dt.get(session.ID); ds != nil {
						t.Fatalf("drain began inside the grace with no suspend: reason=%q", ds.reason)
					}
					if got := inc003GraceRecord(t, trace, name).Fields["drain_reason"]; got != tc.reason {
						t.Errorf("trace drain_reason = %#v, want %q", got, tc.reason)
					}
					return
				}
				inc003DrainTraced(t, env, trace, session, name, tc.reason, scope)
			})
		}
	}
}

// The production suspend path: `gc suspend` and `gc rig suspend` write runtime
// state under the city path, not config. A suspended city drains the row even
// when its agent is gone from config, and a suspended rig drains an agent bound
// to it by name or by path.
func TestReconcileSessionBeads_INC003_RuntimeSuspendDrainsWithoutGrace(t *testing.T) {
	for _, tc := range []struct {
		name, cause    string
		agents         []config.Agent
		rigDir         string
		session, templ string
	}{
		{name: "city", cause: "city", agents: []config.Agent{{Name: "dog"}}, session: "dog-1", templ: "dog"},
		{name: "city-agent-removed", cause: "city", agents: []config.Agent{{Name: "other"}}, session: "gone-1", templ: "gone"},
		{name: "rig", cause: "rig", agents: []config.Agent{{Name: "polecat", Dir: "myrig"}}, rigDir: "myrig", session: "myrig--polecat-1", templ: "myrig/polecat"},
		{name: "rig-path-bound", cause: "rig", agents: []config.Agent{{Name: "polecat", Dir: "rigs/myrig"}}, rigDir: "rigs/myrig", session: "polecat-1", templ: "rigs/myrig/polecat"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			env := newReconcilerTestEnv()
			env.cfg = &config.City{Agents: tc.agents}
			if tc.cause == "city" {
				suspendCityAt(t, cityPath)
			} else {
				env.cfg.Rigs = []config.Rig{{Name: "myrig", Path: filepath.Join(cityPath, tc.rigDir)}}
				suspendRigAt(t, cityPath, "myrig")
			}
			session := inc003LiveRow(t, env, tc.session, tc.templ, nil)
			trace := newPoolDesiredStateTestTrace(tc.session, tc.templ)

			env.reconcileINC003Traced(cityPath, trace, session)

			inc003DrainTraced(t, env, trace, session, tc.session, "orphaned", tc.cause)
		})
	}
}

// Only a suspend that covers the row's own agent counts: another rig suspended
// at runtime and another agent suspended in config leave the grace in place.
func TestReconcileSessionBeads_INC003_OtherScopeSuspendedKeepsGrace(t *testing.T) {
	cityPath := t.TempDir()
	suspendRigAt(t, cityPath, "otherrig")
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Rigs: []config.Rig{{Name: "myrig", Path: filepath.Join(cityPath, "myrig")}, {Name: "otherrig", Path: filepath.Join(cityPath, "otherrig")}},
		Agents: []config.Agent{
			{Name: "polecat", Dir: "myrig"},
			{Name: "polecat", Dir: "otherrig"},
			{Name: "x", Suspended: true},
		},
	}
	session := inc003LiveRow(t, env, "myrig--polecat-1", "myrig/polecat", nil)
	trace := newPoolDesiredStateTestTrace("myrig--polecat-1", "myrig/polecat")

	env.reconcileINC003Traced(cityPath, trace, session)

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("drain began for a row whose own rig and agent are not suspended: %+v", ds)
	}
	inc003GraceRecord(t, trace, "myrig--polecat-1")
}

// A named row whose spec is gone reaches the drain through the #3630 confirm:
// tick 1 defers for confirmation, and tick 2 drains with no grace when the
// operator suspended its agent, or the city with the agent removed.
func TestReconcileSessionBeads_INC003_NamedNoSpecSuspendedDrainsOnConfirm(t *testing.T) {
	for _, tc := range []struct{ name, cause string }{
		{"agent", "agent"},
		{"city-agent-removed", "city"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			env := newReconcilerTestEnv()
			env.cfg = &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "warlord", Suspended: true}}}
			if tc.cause == "city" {
				suspendCityAt(t, cityPath)
				env.cfg.Agents = []config.Agent{{Name: "other"}}
			}
			session := inc003LiveRow(t, env, "warlord", "warlord", map[string]string{
				namedSessionMetadataKey:      "true",
				namedSessionIdentityMetadata: "warlord",
				namedSessionModeMetadata:     "always",
			})
			trace := newPoolDesiredStateTestTrace("warlord")

			env.reconcileINC003Traced(cityPath, trace, session)
			if ds := env.dt.get(session.ID); ds != nil {
				t.Fatalf("tick 1 must be the #3630 confirm deferral, got drain %+v", ds)
			}
			env.reconcileINC003Traced(cityPath, trace, session)

			inc003DrainTraced(t, env, trace, session, "warlord", "orphaned", tc.cause)
		})
	}
}

// Clock skew must never pin a row. A last_woke_at modestly ahead of now (the
// wake can be stamped by another process) is inside the grace, but one
// wakeUndesiredGrace or more ahead gets no grace and drains as before.
func TestReconcileSessionBeads_INC003_FarFutureLastWokeDrains(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ahead     time.Duration
		wantDrain bool
	}{
		{"modest-skew-kept", wakeUndesiredGrace - time.Second, false},
		{"at-grace-drains", wakeUndesiredGrace, true},
		{"far-future-drains", 24 * time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, session := inc003Env(t, false, wokeAgo(-tc.ahead))

			env.reconcile([]beads.Bead{session})

			ds := env.dt.get(session.ID)
			if tc.wantDrain && (ds == nil || ds.reason != "orphaned") {
				t.Fatalf("last_woke_at %s ahead: drain = %+v, want an orphaned drain", tc.ahead, ds)
			}
			if !tc.wantDrain && ds != nil {
				t.Fatalf("last_woke_at %s ahead: drain began (reason=%q), want it kept inside the grace", tc.ahead, ds.reason)
			}
		})
	}
}

// A drain already tracked keeps running inside the grace, and the grace is not
// logged or traced for it: beginSessionDrainInfo would no-op, so a grace record
// would claim a deferral that is not happening.
func TestReconcileSessionBeads_INC003_TrackedDrainNotReportedAsGrace(t *testing.T) {
	env, session := inc003Env(t, false, wokeAgo(30*time.Second))
	env.dt.set(session.ID, &drainState{
		startedAt:  env.clk.Now(),
		deadline:   env.clk.Now().Add(defaultDrainTimeout),
		reason:     "orphaned",
		generation: 1,
	})
	trace := newPoolDesiredStateTestTrace("orphan")

	env.reconcileINC003Traced("", trace, session)

	if ds := env.dt.get(session.ID); ds == nil || ds.reason != "orphaned" {
		t.Fatalf("tracked drain = %+v, want the orphaned drain left in flight", ds)
	}
	for _, r := range trace.records {
		if r.ReasonCode == TraceReasonUndesiredWakeGrace {
			t.Fatalf("grace traced for a row whose drain is already tracked: %+v", r)
		}
	}
	if strings.Contains(env.stdout.String(), "undesired-wake grace") {
		t.Fatalf("grace logged for a row whose drain is already tracked:\n%s", env.stdout.String())
	}
}

// wakeGracePreservesUndesiredRow is the exact rule v2's arm 18 reuses.
func TestWakeGracePreservesUndesiredRow(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	for _, tc := range []struct {
		name, lastWokeAt string
		want             bool
	}{
		{"just-woken", at(0), true},
		{"inside", at(-wakeUndesiredGrace + time.Second), true},
		{"at-grace", at(-wakeUndesiredGrace), false},
		{"modest-future", at(wakeUndesiredGrace - time.Second), true},
		{"future-at-grace", at(wakeUndesiredGrace), false},
		{"empty", "", false},
		{"unparseable", "yesterday", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := sessionpkg.Info{LastWokeAt: tc.lastWokeAt}
			if got := wakeGracePreservesUndesiredRow(info, now); got != tc.want {
				t.Fatalf("wakeGracePreservesUndesiredRow(last_woke_at=%q) = %v, want %v", tc.lastWokeAt, got, tc.want)
			}
		})
	}
}
