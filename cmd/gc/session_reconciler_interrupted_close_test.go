package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// interruptedCloseEnv is a one-agent, max=1 pool city (fake provider, memory
// store) whose only seat can be stamped into the half-closed shape of
// gastownhall/gascity#2085: a two-write close lost its second write, so the
// ClosePatch landed (metadata state carries this binary's own close code)
// while the status flip to closed was lost. The row then reads as an unknown
// state, and the reconciler's unknown-state skip holds it open forever — the
// canonical identity stays pinned and a max=1 pool is stuck at zero seats.
type interruptedCloseEnv struct {
	cityName string
	cityDir  string
	cfg      *config.City
	store    beads.Store
	sp       *runtime.Fake
	clk      *clock.Fake
	dt       *drainTracker
	stderr   *bytes.Buffer
}

const interruptedCloseTemplate = "repo/polecat"

func newInterruptedCloseEnv(t *testing.T) *interruptedCloseEnv {
	t.Helper()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "finish-town"},
		Session:   config.SessionConfig{Provider: "fake"},
		Agents: []config.Agent{{
			Name:              "polecat",
			Dir:               "repo",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(1),
		}},
	}
	return &interruptedCloseEnv{
		cityName: "finish-town",
		cityDir:  t.TempDir(),
		cfg:      cfg,
		store:    beads.NewMemStore(),
		sp:       runtime.NewFake(),
		clk:      &clock.Fake{Time: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)},
		dt:       newDrainTracker(),
		stderr:   &bytes.Buffer{},
	}
}

// halfClose stamps the interrupted close onto the bead: the terminal state
// (and, for a passthrough code, its sleep_reason) this binary's close would
// have written, with the status flip to closed lost.
func (e *interruptedCloseEnv) halfClose(t *testing.T, id, state string, extra map[string]string) {
	t.Helper()
	meta := map[string]string{"state": state}
	for k, v := range extra {
		meta[k] = v
	}
	for k, v := range meta {
		if err := e.store.SetMetadata(id, k, v); err != nil {
			t.Fatalf("SetMetadata(%s=%s): %v", k, v, err)
		}
	}
}

// tick runs one complete non-boot controller pass in the production order:
// desired-state build, session sync (closes deferred to the reconciler), then
// the session reconciler itself.
func (e *interruptedCloseEnv) tick(t *testing.T) {
	t.Helper()
	ds := buildDesiredState(e.cityName, e.cityDir, e.clk.Now(), e.cfg, e.sp, e.store, io.Discard)
	syncSessionBeads(e.cityDir, e.store, ds.State, e.sp, allConfiguredDS(ds.State), e.cfg, e.clk, io.Discard, true)
	rows, err := loadSessionBeads(e.store)
	if err != nil {
		t.Fatalf("load session beads: %v", err)
	}
	reconcileSessionBeadsAtPath(
		context.Background(), e.cityDir, rows, ds.State, map[string]bool{interruptedCloseTemplate: true},
		e.cfg, e.sp, e.store, newDrainOps(e.sp), nil, nil, nil, e.dt, ds.PoolDesiredCounts, false, nil, e.cityName,
		nil, e.clk, events.Discard, 0, 0, io.Discard, e.stderr,
	)
}

// openSeatRunning reports whether some open session bead of the template has
// a running runtime — the pool is serving seats again.
func (e *interruptedCloseEnv) openSeatRunning(t *testing.T) bool {
	t.Helper()
	rows, err := loadSessionBeads(e.store)
	if err != nil {
		t.Fatalf("load session beads: %v", err)
	}
	for _, b := range rows {
		if b.Status != "open" || b.Metadata["template"] != interruptedCloseTemplate {
			continue
		}
		if name := b.Metadata["session_name"]; name != "" && e.sp.IsRunning(name) {
			return true
		}
	}
	return false
}

// halfClosedSeat stamps the interrupted close onto a freshly minted pool
// seat and returns the re-read bead.
func (e *interruptedCloseEnv) halfClosedSeat(t *testing.T, state string) beads.Bead {
	t.Helper()
	seat := createCanonicalPoolSession(t, e.store, &e.cfg.Agents[0], e.clk.Now(), 1)
	e.halfClose(t, seat.ID, state, nil)
	stored, err := e.store.Get(seat.ID)
	if err != nil {
		t.Fatalf("reload half-closed seat: %v", err)
	}
	return stored
}

// tracedTick runs one traced reconcile pass over sessions against store and
// provider sp (either may wrap the env's own, to inject a liveness failure),
// with the template armed for detail so decisions land on the cycle.
func (e *interruptedCloseEnv) tracedTick(t *testing.T, sessions []beads.Bead, store beads.Store, sp runtime.Provider, storeQueryPartial bool, startOpts ...startExecutionOption) (*SessionReconcilerTraceCycle, *events.Fake) {
	t.Helper()
	tracer := newSessionReconcilerTracer(e.cityDir, e.cityName, io.Discard)
	armNow := e.clk.Now()
	if _, err := tracer.armStore.upsertArm(TraceArm{
		ScopeType:      TraceArmScopeTemplate,
		ScopeValue:     interruptedCloseTemplate,
		Source:         TraceArmSourceManual,
		Level:          TraceModeDetail,
		ArmedAt:        armNow,
		ExpiresAt:      armNow.Add(15 * time.Minute),
		LastExtendedAt: armNow,
		UpdatedAt:      armNow,
	}); err != nil {
		t.Fatalf("upsert arm: %v", err)
	}
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", armNow, e.cfg)
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	cycle.syncArms(armNow, e.cfg)
	rec := events.NewFake()
	reconcileSessionBeadsTraced(
		context.Background(), e.cityDir, sessions, nil, map[string]bool{interruptedCloseTemplate: true},
		e.cfg, sp, store, nil, nil, nil, nil, e.dt, nil, storeQueryPartial, nil, e.cityName,
		nil, e.clk, rec, 0, 0, io.Discard, e.stderr, cycle,
		startOpts...,
	)
	if err := tracer.Close(); err != nil {
		t.Fatalf("tracer close: %v", err)
	}
	return cycle, rec
}

// finishInterruptedCloseDecisions returns the finisher's decision records
// from the cycle.
func finishInterruptedCloseDecisions(cycle *SessionReconcilerTraceCycle) []SessionReconcilerTraceRecord {
	var out []SessionReconcilerTraceRecord
	for _, rec := range cycle.records {
		if rec.RecordType == TraceRecordDecision && rec.SiteCode == TraceSiteReconcilerFinishInterruptedClose {
			out = append(out, rec)
		}
	}
	return out
}

// unknownStateSkipRecorded reports whether the tick recorded the skip site's
// unknown_state_skipped decision — S20's trace half, exactly as at base.
func unknownStateSkipRecorded(cycle *SessionReconcilerTraceCycle) bool {
	for _, rec := range cycle.records {
		if rec.SiteCode == TraceSiteReconcilerUnknownState && rec.ReasonCode == TraceReasonUnknownStateSkipped {
			return true
		}
	}
	return false
}

// unknownStateEvents returns the session.unknown_state events the tick
// emitted.
func unknownStateEvents(t *testing.T, rec *events.Fake) []events.Event {
	t.Helper()
	evs, err := rec.List(events.Filter{Type: events.SessionUnknownState})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	return evs
}

// TestReconcileSessionBeads_InterruptedCloseRefusesLiveOrUnobservableRuntime
// is the AC-3 guard: the finisher never finishes a close it cannot observe.
// A half-closed stale-session row whose runtime is alive (or whose liveness
// probe errors) must stay open with its state unchanged, record kept_open or
// skipped_liveness_error at reconciler.session.finish_interrupted_close, and
// leave S20 exactly as at base — while no runtime is stopped, including a
// live twin's: the release path sits behind the liveness verdict.
func TestReconcileSessionBeads_InterruptedCloseRefusesLiveOrUnobservableRuntime(t *testing.T) {
	t.Run("alive runtime keeps the row open", func(t *testing.T) {
		env := newInterruptedCloseEnv(t)
		zombie := env.halfClosedSeat(t, "stale-session")
		name := zombie.Metadata["session_name"]
		// The name is live — whether the row's own runtime or a twin's: the
		// probe reads the name.
		if err := env.sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatalf("start runtime under %q: %v", name, err)
		}

		cycle, rec := env.tracedTick(t, []beads.Bead{zombie}, env.store, env.sp, false)

		got, err := env.store.Get(zombie.ID)
		if err != nil {
			t.Fatalf("reload zombie: %v", err)
		}
		if got.Status != "open" || got.Metadata["state"] != "stale-session" {
			t.Fatalf("live half-closed row changed: status=%q state=%q, want open/stale-session", got.Status, got.Metadata["state"])
		}
		decisions := finishInterruptedCloseDecisions(cycle)
		if len(decisions) != 1 || decisions[0].OutcomeCode != TraceOutcomeKeptOpen {
			t.Fatalf("finisher decisions = %+v, want one kept_open", decisions)
		}
		if !unknownStateSkipRecorded(cycle) {
			t.Fatal("S20's unknown_state_skipped decision missing — the skip site must still signal a row the finisher declines")
		}
		if len(unknownStateEvents(t, rec)) != 1 {
			t.Fatal("session.unknown_state not emitted exactly as at base (first sight)")
		}
		if got.Metadata["unknown_state_first_seen"] == "" {
			t.Fatal("S20 first-seen marker not stamped")
		}
		if !env.sp.IsRunning(name) || env.sp.CountCalls("Stop", name) != 0 {
			t.Fatalf("runtime under %q was stopped or died: running=%v stop_calls=%d", name, env.sp.IsRunning(name), env.sp.CountCalls("Stop", name))
		}
	})

	t.Run("liveness error skips the close", func(t *testing.T) {
		env := newInterruptedCloseEnv(t)
		zombie := env.halfClosedSeat(t, "stale-session")
		name := zombie.Metadata["session_name"]
		// The provider cannot observe liveness — the same class a transient
		// tmux blip produces at runtime.
		sp := &startUnavailableLivenessProvider{Fake: env.sp}

		cycle, rec := env.tracedTick(t, []beads.Bead{zombie}, env.store, sp, false)

		got, err := env.store.Get(zombie.ID)
		if err != nil {
			t.Fatalf("reload zombie: %v", err)
		}
		if got.Status != "open" || got.Metadata["state"] != "stale-session" {
			t.Fatalf("unobservable half-closed row changed: status=%q state=%q, want open/stale-session", got.Status, got.Metadata["state"])
		}
		decisions := finishInterruptedCloseDecisions(cycle)
		if len(decisions) != 1 || decisions[0].OutcomeCode != TraceOutcomeSkippedLivenessError {
			t.Fatalf("finisher decisions = %+v, want one skipped_liveness_error", decisions)
		}
		if !strings.Contains(env.stderr.String(), "skipping interrupted close of '"+name+"': liveness observation failed") {
			t.Fatalf("fail-closed stderr line missing; stderr=%q", env.stderr.String())
		}
		if !unknownStateSkipRecorded(cycle) {
			t.Fatal("S20's unknown_state_skipped decision missing — the skip site must still signal a row the finisher declines")
		}
		if len(unknownStateEvents(t, rec)) != 1 {
			t.Fatal("session.unknown_state not emitted exactly as at base (first sight)")
		}
		if env.sp.CountCalls("Stop", name) != 0 {
			t.Fatalf("runtime under %q stopped despite liveness error: stop_calls=%d", name, env.sp.CountCalls("Stop", name))
		}
	})
}

// TestReconcileSessionBeads_InterruptedCloseDefersOnPartialObservation is the
// AC-4 guard: a tick whose observation is partial (storeQueryPartial) or whose
// closes are deferred on the boot reconcile does not finish an interrupted
// close — it stays open, records skipped with both flags, and the next
// complete non-boot tick closes it.
func TestReconcileSessionBeads_InterruptedCloseDefersOnPartialObservation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		partial     bool
		startOpt    startExecutionOption
		wantPartial bool
		wantDefer   bool
	}{
		{name: "store query partial", partial: true, wantPartial: true, wantDefer: false},
		{name: "defer session closes on boot", startOpt: withDeferSessionClosesOnBoot(), wantPartial: false, wantDefer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newInterruptedCloseEnv(t)
			zombie := env.halfClosedSeat(t, "stale-session")

			var cycle *SessionReconcilerTraceCycle
			if tc.startOpt != nil {
				cycle, _ = env.tracedTick(t, []beads.Bead{zombie}, env.store, env.sp, tc.partial, tc.startOpt)
			} else {
				cycle, _ = env.tracedTick(t, []beads.Bead{zombie}, env.store, env.sp, tc.partial)
			}

			got, err := env.store.Get(zombie.ID)
			if err != nil {
				t.Fatalf("reload zombie: %v", err)
			}
			if got.Status != "open" {
				t.Fatalf("half-closed seat closed on a partial/deferred tick: status=%q, want open", got.Status)
			}
			decisions := finishInterruptedCloseDecisions(cycle)
			if len(decisions) != 1 || decisions[0].OutcomeCode != TraceOutcomeSkipped {
				t.Fatalf("finisher decisions = %+v, want one skipped", decisions)
			}
			fields := decisions[0].Fields
			if fields["store_query_partial"] != tc.wantPartial || fields["defer_session_closes_on_boot"] != tc.wantDefer {
				t.Fatalf("skipped decision fields = %#v, want store_query_partial=%v defer_session_closes_on_boot=%v",
					fields, tc.wantPartial, tc.wantDefer)
			}

			// The next complete non-boot tick finishes the close.
			env.tick(t)
			closed, err := env.store.Get(zombie.ID)
			if err != nil {
				t.Fatalf("reload zombie after complete tick: %v", err)
			}
			if closed.Status != "closed" || closed.Metadata["state"] != "stale-session" {
				t.Fatalf("complete tick did not finish the close: status=%q state=%q", closed.Status, closed.Metadata["state"])
			}
		})
	}
}

// TestReconcileSessionBeads_InterruptedCloseHonorsWorkGates is the AC-5 guard:
// the finisher's close takes the recognizer's gate, so the gates mean what
// they mean everywhere else. A closeGated code (duplicate) refuses while the
// seat still holds in_progress assigned work — the row stays open and the
// work keeps its assignee; a closeReleasing code (stale-session) releases the
// seat's work instead of gating on it — the row closes and the work reopens
// unassigned for the pool to re-route.
func TestReconcileSessionBeads_InterruptedCloseHonorsWorkGates(t *testing.T) {
	inProgressWorkOn := func(t *testing.T, env *interruptedCloseEnv, seat beads.Bead) beads.Bead {
		t.Helper()
		work, err := env.store.Create(beads.Bead{
			Title:  "held work",
			Status: "in_progress",
			Metadata: map[string]string{
				"gc.routed_to": interruptedCloseTemplate,
			},
		})
		if err != nil {
			t.Fatalf("create work: %v", err)
		}
		assignee := seat.ID
		if err := env.store.Update(work.ID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
			t.Fatalf("assign work to seat: %v", err)
		}
		return work
	}

	t.Run("closeGated duplicate refuses while work is held", func(t *testing.T) {
		env := newInterruptedCloseEnv(t)
		zombie := env.halfClosedSeat(t, "duplicate")
		work := inProgressWorkOn(t, env, zombie)

		cycle, _ := env.tracedTick(t, []beads.Bead{zombie}, env.store, env.sp, false)

		got, err := env.store.Get(zombie.ID)
		if err != nil {
			t.Fatalf("reload zombie: %v", err)
		}
		if got.Status != "open" {
			t.Fatalf("closeGated half-closed seat closed while holding work: status=%q, want open", got.Status)
		}
		stored, err := env.store.Get(work.ID)
		if err != nil {
			t.Fatalf("reload work: %v", err)
		}
		if stored.Assignee != zombie.ID {
			t.Fatalf("held work lost its assignee under a refused close: assignee=%q, want %q",
				stored.Assignee, zombie.ID)
		}
		decisions := finishInterruptedCloseDecisions(cycle)
		if len(decisions) != 1 || decisions[0].OutcomeCode != TraceOutcomeNoChange {
			t.Fatalf("finisher decisions = %+v, want one no_change (gate refused)", decisions)
		}
	})

	t.Run("closeReleasing stale-session releases the work", func(t *testing.T) {
		env := newInterruptedCloseEnv(t)
		zombie := env.halfClosedSeat(t, "stale-session")
		work := inProgressWorkOn(t, env, zombie)

		env.tracedTick(t, []beads.Bead{zombie}, env.store, env.sp, false)

		got, err := env.store.Get(zombie.ID)
		if err != nil {
			t.Fatalf("reload zombie: %v", err)
		}
		if got.Status != "closed" || got.Metadata["state"] != "stale-session" {
			t.Fatalf("closeReleasing half-closed seat not closed: status=%q state=%q", got.Status, got.Metadata["state"])
		}
		stored, err := env.store.Get(work.ID)
		if err != nil {
			t.Fatalf("reload work: %v", err)
		}
		if stored.Status != "open" || stored.Assignee != "" {
			t.Fatalf("released work not reopened unassigned: status=%q assignee=%q, want open/\"\"",
				stored.Status, stored.Assignee)
		}
	})
}

// TestReconcileSessionBeads_InterruptedCloseFreesAlwaysNamedSession is the
// AC-2 companion of the pool-seat guard: the same half-closed row (state
// stale-session, no runtime) on an always-mode configured named session. The
// named identity is controller-managed every tick, but an open bead owns its
// session_name, so the half-closed row pins the identity forever. One
// complete non-boot tick finishes the close; the closed row's own close code
// refuses the configured-named reopen (closedNamedSessionReopenEligible), so
// the identity must come back on a DIFFERENT open bead with its runtime
// running — and the closed row stays closed.
func TestReconcileSessionBeads_InterruptedCloseFreesAlwaysNamedSession(t *testing.T) {
	env := newInterruptedCloseEnv(t)
	// Reconfigure as a named-session city: one agent template, one
	// mode=always named session over it.
	env.cfg.Agents = []config.Agent{{Name: "warlord", StartCommand: "true"}}
	env.cfg.NamedSessions = []config.NamedSession{{Template: "warlord", Mode: "always"}}
	sessionName := config.NamedSessionRuntimeName(env.cityName, env.cfg.Workspace, "warlord")

	zombie, err := env.store.Create(beads.Bead{
		Title:  "warlord",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":                   "warlord",
			"agent_name":                 "warlord",
			"session_name":               sessionName,
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "warlord",
			namedSessionModeMetadata:     "always",
		},
	})
	if err != nil {
		t.Fatalf("create named zombie: %v", err)
	}
	env.halfClose(t, zombie.ID, "stale-session", nil)

	// One complete non-boot tick finishes the interrupted close.
	env.tick(t)

	got, err := env.store.Get(zombie.ID)
	if err != nil {
		t.Fatalf("reload named zombie: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("half-closed named session status = %q, want closed after one tick; metadata=%v", got.Status, got.Metadata)
	}
	if got.Metadata["state"] != "stale-session" {
		t.Fatalf("closed named bead state = %q, want stale-session", got.Metadata["state"])
	}

	// The identity comes back on a different open bead within 6 ticks.
	freshRunning := func() bool {
		rows, err := loadSessionBeads(env.store)
		if err != nil {
			t.Fatalf("load session beads: %v", err)
		}
		for _, b := range rows {
			if b.Status != "open" || b.ID == zombie.ID {
				continue
			}
			if b.Metadata["session_name"] == sessionName && env.sp.IsRunning(sessionName) {
				return true
			}
		}
		return false
	}
	for i := 0; i < 5 && !freshRunning(); i++ {
		env.tick(t)
	}
	if !freshRunning() {
		rows, _ := loadSessionBeads(env.store)
		t.Fatalf("named identity %q not running on a fresh open bead after the interrupted close; session beads: %+v", sessionName, rows)
	}

	// The finished close is terminal: later ticks do not resurrect it.
	stored, err := env.store.Get(zombie.ID)
	if err != nil {
		t.Fatalf("reload named zombie after later ticks: %v", err)
	}
	if stored.Status != "closed" {
		t.Fatalf("closed named bead status = %q after later ticks, want closed (stays closed)", stored.Status)
	}
}

// TestReconcileSessionBeads_InterruptedCloseFreesMaxOnePoolSeat is the AC-1
// regression guard for gastownhall/gascity#2085. A max=1 pool's only seat is
// half-closed — the close's ClosePatch landed (state carries this binary's
// own close code) but the status flip to closed was lost — so the row reads
// as an unknown state. One complete non-boot controller tick must finish the
// interrupted close: the bead closes carrying its own close code and the
// canonical close reason, and with the identity freed the pool mints and
// starts a replacement seat to serve ready routed work.
func TestReconcileSessionBeads_InterruptedCloseFreesMaxOnePoolSeat(t *testing.T) {
	cases := []struct {
		name  string
		state string
		extra map[string]string
	}{
		{name: "duplicate", state: "duplicate"},
		{name: "reconfigured", state: "reconfigured"},
		{name: "gc_swept", state: "gc_swept"},
		{name: "stale-session", state: "stale-session"},
		{name: "dead-runtime", state: "dead-runtime"},
		{name: "stranded-repair", state: "stranded-repair"},
		{name: "idle-timeout passthrough", state: "idle-timeout", extra: map[string]string{"sleep_reason": "idle-timeout"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newInterruptedCloseEnv(t)
			zombie := createCanonicalPoolSession(t, env.store, &env.cfg.Agents[0], env.clk.Now(), 1)
			env.halfClose(t, zombie.ID, tc.state, tc.extra)
			createRoutedReadyBeadForReplacement(t, env.store, interruptedCloseTemplate, "queued work")

			// One complete non-boot tick finishes the interrupted close.
			env.tick(t)

			got, err := env.store.Get(zombie.ID)
			if err != nil {
				t.Fatalf("reload zombie: %v", err)
			}
			if got.Status != "closed" {
				t.Fatalf("half-closed seat (state=%q) status = %q, want closed after one tick; metadata=%v",
					tc.state, got.Status, got.Metadata)
			}
			if got.Metadata["state"] != tc.state {
				t.Fatalf("closed bead state = %q, want the interrupted close's own code %q", got.Metadata["state"], tc.state)
			}
			if got.Metadata["close_reason"] != sessionpkg.CanonicalCloseReason(tc.state) {
				t.Fatalf("closed bead close_reason = %q, want %q", got.Metadata["close_reason"], sessionpkg.CanonicalCloseReason(tc.state))
			}

			// With the canonical identity freed, the pool serves the queued
			// bead: an open seat of the template runs again within a few more
			// complete ticks.
			for i := 0; i < 5 && !env.openSeatRunning(t); i++ {
				env.tick(t)
			}
			if !env.openSeatRunning(t) {
				rows, _ := loadSessionBeads(env.store)
				t.Fatalf("no open running seat of %s after the interrupted close freed the identity; session beads: %+v",
					interruptedCloseTemplate, rows)
			}
		})
	}
}
