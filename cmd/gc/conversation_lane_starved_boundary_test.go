package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// 21q: the lane's parity with the legacy poller for a session whose
// activity clock never quiets (#8544) and for the Codex mid-turn hold
// (#8840). The quiescence rule alone holds a keystroke runtime's boundary
// closed while its pane keeps drawing (an idle TUI that repaints, back-to-back
// turns), so lane content to such a session would wait until its start_by
// closed it undelivered. Content that has been deliverable for
// laneBoundaryStarvedAfter asks the screen's turn look instead, as the poller's
// starved queue does. For a provider that declares hold_mid_turn that bypass
// goes only under program_status = "on", on a fresh reading of the program's
// record that decides and shows no turn working; under any other switch, or
// on a reading that decides nothing, its content waits for the quiescence
// rule.

// starvedLane is a lane over one conversation whose pane never quiets: every
// pass reads its last activity as now.
type starvedLane struct {
	f    *laneFixture
	deps laneExecDeps
	st   *opjournal.Store
	l    *opjournal.ConversationLane
	// looks counts the starved boundary's looks.
	looks int
	// beforeLook, when set, runs ahead of each look, given its count.
	beforeLook func(look int)
}

func newStarvedLane(t *testing.T, provider string, opts ...laneFixtureOpt) *starvedLane {
	t.Helper()
	f, deps, st := newScreenTurnFixture(t, provider, opts...)
	f.showTurn(false)
	return &starvedLane{f: f, deps: deps, st: st, l: f.lane(t, st)}
}

// providerSessionRow makes a fixture's row a provider session's (API or
// dashboard kind=provider): its template is its provider's name, it names
// no agent, and it was made by hand.
func providerSessionRow(meta map[string]string) {
	delete(meta, "agent_name")
	meta["template"] = meta["provider"]
	meta["session_origin"] = "manual"
}

// codexCatalogCity is a city under the program_status switch mode whose
// catalog holds the builtin codex profile, which declares hold_mid_turn,
// and no agent. Its path check names a binary every test host has, so the
// production resolution (exec.LookPath) resolves it.
func codexCatalogCity(mode string) *config.City {
	return &config.City{
		Session:   config.SessionConfig{ProgramStatus: mode},
		Providers: map[string]config.ProviderSpec{"codex": {Base: stringPtr("builtin:codex"), PathCheck: "true"}},
	}
}

// codexAgentCity is codexCatalogCity with the fixture's agent, worker,
// running codex.
func codexAgentCity(mode string) *config.City {
	cfg := codexCatalogCity(mode)
	cfg.Agents = []config.Agent{{Name: "worker", Provider: "codex"}}
	return cfg
}

// prompt proposes a boundary prompt (gc session nudge's wait-idle) now.
func (s *starvedLane) prompt(t *testing.T, key, payload string) opjournal.Op {
	t.Helper()
	return s.f.propose(t, s.st, s.f.msg(key, opjournal.RolePrompt, payload, func(r *opjournal.Request) {
		r.Policy = cliLaneNudgePolicy(nudgeDeliveryWaitIdle)
		r.Producer = opjournal.Producer{Principal: "operator", Actor: opjournal.ActorOperator}
	}))
}

// pass runs one lane pass with the pane's activity reading now, so the
// quiescence rule reads it busy, and returns op as the pass left it.
func (s *starvedLane) pass(t *testing.T, op opjournal.Op) opjournal.Op {
	t.Helper()
	return s.passActiveAt(t, op, s.f.clk.Now())
}

// passQuiet runs one lane pass with the pane drawing nothing for a minute,
// so the quiescence rule reads it quiet, and returns op as the pass left it.
func (s *starvedLane) passQuiet(t *testing.T, op opjournal.Op) opjournal.Op {
	t.Helper()
	return s.passActiveAt(t, op, s.f.clk.Now().Add(-time.Minute))
}

// passActiveAt runs one lane pass with the pane's last activity at at and
// returns op as the pass left it.
func (s *starvedLane) passActiveAt(t *testing.T, op opjournal.Op, at time.Time) opjournal.Op {
	t.Helper()
	s.f.sp.SetActivity(s.f.name, at)
	v, row := s.f.view(t)
	e := newConversationLaneExec(s.deps, row)
	quiescenceOnFixtureClock(s.f, e)
	look := e.starvedHold
	e.starvedHold = func(t nudgeTarget, now time.Time) (bool, error) {
		s.looks++
		if s.beforeLook != nil {
			s.beforeLook(s.looks)
		}
		return look(t, now)
	}
	if _, err := s.l.Pass(context.Background(), v, e); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	return s.f.get(t, s.st, op.ID)
}

// screenLooks counts the starved boundary's looks at the session's turn.
func (s *starvedLane) screenLooks() int {
	return s.looks
}

// TestLaneContentToANeverQuietSessionDeliversAfterStarvation: a boundary
// prompt to a session whose pane draws on every look and whose screen shows
// no turn waits while it is fresh and is typed once, as soon as it has been
// deliverable for laneBoundaryStarvedAfter.
func TestLaneContentToANeverQuietSessionDeliversAfterStarvation(t *testing.T) {
	s := newStarvedLane(t, "claude")
	const payload = "blocker gc-1 closed — routed dependent gc-2 may be ready to claim"
	op := s.prompt(t, "k-starved", payload)
	proposed := s.f.clk.Now()
	var got opjournal.Op
	for range 40 {
		if got = s.pass(t, op); got.Status == opjournal.StatusClosed {
			break
		}
		s.f.clk.Advance(laneBusyMax)
	}
	waited := s.f.clk.Now().Sub(proposed)
	if got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("after %s of a never-quiet pane: op %s %+v; want it closed succeeded once starved", waited, got.Status, got.Outcome)
	}
	if waited < laneBoundaryStarvedAfter || waited > laneBoundaryStarvedAfter+laneBusyMax {
		t.Fatalf("delivered after %s; want within one busy backoff of %s", waited, laneBoundaryStarvedAfter)
	}
	if typed := s.f.nudges(); len(typed) != 1 || !strings.Contains(typed[0].Message, payload) {
		t.Fatalf("typed %+v; want the prompt once", typed)
	}
}

// TestLaneFreshBusyDeferralStillWaits: short of laneBoundaryStarvedAfter a
// busy pane's content waits as before, and nothing reads the screen for it.
func TestLaneFreshBusyDeferralStillWaits(t *testing.T) {
	s := newStarvedLane(t, "claude")
	op := s.prompt(t, "k-fresh", "fresh")
	for range 11 {
		if got := s.pass(t, op); got.Status == opjournal.StatusClosed {
			t.Fatalf("op closed %+v short of starvation; want it waiting", got.Outcome)
		}
		s.f.clk.Advance(laneBusyMax)
	}
	if typed := s.f.nudges(); len(typed) != 0 {
		t.Fatalf("typed %+v into a busy pane short of starvation; want nothing", typed)
	}
	if n := s.screenLooks(); n != 0 {
		t.Fatalf("%d screen looks short of starvation; want none", n)
	}
}

// TestLaneStarvedBoundaryHoldsARunningTurn: a starved prompt to a session
// whose screen shows a running turn is not typed into it; once the screen
// shows none, it is typed once.
func TestLaneStarvedBoundaryHoldsARunningTurn(t *testing.T) {
	s := newStarvedLane(t, "claude")
	s.f.showTurn(true)
	const payload = "starved while a turn runs"
	op := s.prompt(t, "k-turn", payload)
	s.f.clk.Advance(10 * time.Minute)
	for range 6 {
		if got := s.pass(t, op); got.Status == opjournal.StatusClosed {
			t.Fatalf("op closed %+v while the screen shows a turn; want it waiting", got.Outcome)
		}
		s.f.clk.Advance(laneBusyMax)
	}
	if typed := s.f.nudges(); len(typed) != 0 {
		t.Fatalf("typed %+v into a running turn; want nothing", typed)
	}
	if s.screenLooks() == 0 {
		t.Fatal("a starved prompt read no screen; want the turn look to decide")
	}
	s.f.showTurn(false)
	if got := s.pass(t, op); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("after the turn: op %s %+v; want closed succeeded", got.Status, got.Outcome)
	}
	if typed := s.f.nudges(); len(typed) != 1 || !strings.Contains(typed[0].Message, payload) {
		t.Fatalf("typed %+v; want the prompt once", typed)
	}
}

// TestLaneStarvedDeliveryRecordsNoIdleWait: under the shadow switch a
// starved prompt's bypass is recorded at starved_bypass, beside the
// record's verdict, and its delivery is not recorded again as an idle wait
// returning (wait_for_idle): no idle wait let it go. A prompt the quiescence
// rule lets go is recorded at wait_for_idle, and at no starved_bypass.
func TestLaneStarvedDeliveryRecordsNoIdleWait(t *testing.T) {
	deliver := func(t *testing.T, starved bool) map[runtime.ProgramStatusPoint]int {
		t.Helper()
		s := newStarvedLane(t, "claude")
		s.deps.cfg = &config.City{Session: config.SessionConfig{ProgramStatus: "shadow"}}
		s.f.sp.SetProgramPane(s.f.name, holdPaneRow(runtime.ProgramStatusShadow, runtime.ProgramIdle, time.Now()))
		points := map[runtime.ProgramStatusPoint]int{}
		s.f.sp.ProgramShadowSink = func(cmp runtime.ProgramStatusComparison) { points[cmp.Point]++ }
		op := s.prompt(t, "k-shadow", "delivered under the shadow")
		pass := s.passQuiet
		if starved {
			s.f.clk.Advance(10 * time.Minute)
			pass = s.pass
		}
		if got := pass(t, op); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded {
			t.Fatalf("op %s %+v; want closed succeeded", got.Status, got.Outcome)
		}
		return points
	}
	if points := deliver(t, true); points[runtime.ProgramPointStarvedBypass] == 0 || points[runtime.ProgramPointWaitForIdle] != 0 {
		t.Fatalf("a starved delivery recorded %v; want starved_bypass and no wait_for_idle", points)
	}
	if points := deliver(t, false); points[runtime.ProgramPointWaitForIdle] != 1 || points[runtime.ProgramPointStarvedBypass] != 0 {
		t.Fatalf("a quiet delivery recorded %v; want one wait_for_idle and no starved_bypass", points)
	}
}

// TestLaneStarvedBoundaryHonorsTheCodexHold: under program_status = "on" a
// provider declaring hold_mid_turn has the starved bypass vetoed while a
// fresh read of its program's record says a turn is working, though the
// screen shows none, and held, unnoted, while no fresh read decides. Under
// any other switch no record is read to veto the screen's look, which
// misses running Codex turns (#8840), so its starved content is never typed
// on that look: nothing looks at the turn or reads the record for it, and
// the quiescence rule alone delivers it. The declaration reaches the lane
// through provider resolution, for a session of an agent that runs codex and
// for a provider session (API or dashboard kind=provider), which names no
// agent.
func TestLaneStarvedBoundaryHonorsTheCodexHold(t *testing.T) {
	for _, kind := range []struct {
		name string
		row  laneFixtureOpt
		city func(mode string) *config.City
	}{
		{"agent session", func(map[string]string) {}, codexAgentCity},
		{"provider session", providerSessionRow, codexCatalogCity},
	} {
		setup := func(t *testing.T, city string) (*starvedLane, opjournal.Op) {
			t.Helper()
			s := newStarvedLane(t, "codex", kind.row)
			s.deps.cfg = kind.city(city)
			s.f.sp.SetProgramPane(s.f.name, holdPaneRow(runtime.ProgramStatusOn, runtime.ProgramWorking, time.Now()))
			op := s.prompt(t, "k-codex", "starved into a codex turn")
			s.f.clk.Advance(10 * time.Minute)
			return s, op
		}
		t.Run(kind.name, func(t *testing.T) {
			t.Run("on", func(t *testing.T) {
				s, op := setup(t, "on")
				for range 3 {
					if got := s.pass(t, op); got.Status == opjournal.StatusClosed {
						t.Fatalf("op closed %+v while the record says working; want it held", got.Outcome)
					}
					s.f.clk.Advance(laneBusyMax)
				}
				if typed := s.f.nudges(); len(typed) != 0 {
					t.Fatalf("typed %+v while the record says working; want nothing", typed)
				}
				if n := s.f.sp.CountCalls("ProgramStatusHoldRead", s.f.name); n == 0 {
					t.Fatal("no hold read; want the record read fresh before the bypass")
				}
				s.f.sp.SetProgramPane(s.f.name, holdPaneRow(runtime.ProgramStatusOn, runtime.ProgramDone, time.Now()))
				if got := s.pass(t, op); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded {
					t.Fatalf("after the record says done: op %s %+v; want closed succeeded", got.Status, got.Outcome)
				}
				if typed := s.f.nudges(); len(typed) != 1 {
					t.Fatalf("typed %+v; want the prompt once", typed)
				}
			})
			// Under on, a fresh reading that decides nothing holds as no
			// record does under off: the screen's look alone never types
			// into such a provider's pane. Each pane would let the prompt go
			// on its record (done) if its reading decided.
			t.Run("on undecided", func(t *testing.T) {
				for _, tc := range []struct {
					name string
					pane func(runtime.ProgramPaneRow) runtime.ProgramPaneRow
				}{
					{"pane undeclared", func(r runtime.ProgramPaneRow) runtime.ProgramPaneRow { r.HoldDeclared = false; return r }},
					{"pane not armed", func(r runtime.ProgramPaneRow) runtime.ProgramPaneRow { r.Tap = ""; return r }},
					{"reader lost", func(r runtime.ProgramPaneRow) runtime.ProgramPaneRow { r.PanePipe = false; return r }},
					{"record silent", func(r runtime.ProgramPaneRow) runtime.ProgramPaneRow {
						r.Record = runtime.ProgramStatus{Nonce: r.Tap}
						return r
					}},
					{"record cleared", func(r runtime.ProgramPaneRow) runtime.ProgramPaneRow {
						r.Record = runtime.ProgramStatus{Nonce: r.Tap, Reported: true}
						return r
					}},
				} {
					t.Run(tc.name, func(t *testing.T) {
						s, op := setup(t, "on")
						noted := 0
						s.deps.note = func(stage string, _ error) {
							if stage == "boundary_starved" {
								noted++
							}
						}
						done := holdPaneRow(runtime.ProgramStatusOn, runtime.ProgramDone, time.Now())
						s.f.sp.SetProgramPane(s.f.name, tc.pane(done))
						for range 3 {
							if got := s.pass(t, op); got.Status == opjournal.StatusClosed {
								t.Fatalf("op closed %+v on a reading that decides nothing; want it held", got.Outcome)
							}
							s.f.clk.Advance(laneBusyMax)
						}
						if typed := s.f.nudges(); len(typed) != 0 || s.screenLooks() == 0 || noted != 0 {
							t.Fatalf("typed %+v after %d starved looks, %d noted; want nothing typed or noted after a look", typed, s.screenLooks(), noted)
						}
						s.f.sp.SetProgramPane(s.f.name, done)
						if got := s.pass(t, op); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded {
							t.Fatalf("on a deciding reading: op %s %+v; want closed succeeded", got.Status, got.Outcome)
						}
					})
				}
			})
			for _, tc := range []struct{ name, city string }{{"off", "off"}, {"shadow", "shadow"}, {"unset", ""}} {
				t.Run(tc.name, func(t *testing.T) {
					s, op := setup(t, tc.city)
					for range 3 {
						if got := s.pass(t, op); got.Status == opjournal.StatusClosed {
							t.Fatalf("switch %q: op closed %+v on the screen's look alone; want it waiting for the quiescence rule", tc.city, got.Outcome)
						}
						s.f.clk.Advance(laneBusyMax)
					}
					if typed := s.f.nudges(); len(typed) != 0 {
						t.Fatalf("switch %q: typed %+v while the record says working; want nothing", tc.city, typed)
					}
					if looks, holds := s.screenLooks(), s.f.sp.CountCalls("ProgramStatusHoldRead", s.f.name); looks != 0 || holds != 0 {
						t.Fatalf("switch %q: %d starved looks, %d hold reads; want none", tc.city, looks, holds)
					}
					if got := s.passQuiet(t, op); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded {
						t.Fatalf("switch %q, the pane quiet: op %s %+v; want the quiescence rule to deliver it", tc.city, got.Status, got.Outcome)
					}
					if typed := s.f.nudges(); len(typed) != 1 {
						t.Fatalf("switch %q: typed %+v; want the prompt once", tc.city, typed)
					}
				})
			}
		})
	}
}

// TestLaneStarvedBoundaryVetoedAtTheSendLook: Deliver takes the starved look
// again right before it types, as the legacy poller's admit does before its
// send (TestStarvedBypassVetoedAtTheSendLook), because the probe's look came
// before the Take. A Codex turn that starts in between, as when Codex picks
// up a follow-up it queued, holds the delivery: nothing is typed, and the op
// is handed back as transient, uncharged and not delivered. A record that no
// longer decides, a turn the screen now shows and a screen read that fails
// hold it the same way. Once the look lets it go, a later pass types the
// prompt once.
func TestLaneStarvedBoundaryVetoedAtTheSendLook(t *testing.T) {
	codexPane := func(s *starvedLane, st runtime.ProgramState, mod func(*runtime.ProgramPaneRow)) {
		r := holdPaneRow(runtime.ProgramStatusOn, st, time.Now())
		if mod != nil {
			mod(&r)
		}
		s.f.sp.SetProgramPane(s.f.name, r)
	}
	for _, tc := range []struct {
		name     string
		provider string
		city     *config.City
		// ready lets the starved prompt go; change, made between the
		// probe's look and Deliver's, holds it.
		ready, change func(s *starvedLane)
		// reads is how many record reads the held pass takes.
		reads int
	}{
		{
			name: "record working", provider: "codex", city: codexAgentCity("on"),
			ready:  func(s *starvedLane) { codexPane(s, runtime.ProgramDone, nil) },
			change: func(s *starvedLane) { codexPane(s, runtime.ProgramWorking, nil) },
			reads:  2,
		},
		{
			name: "record no longer decides", provider: "codex", city: codexAgentCity("on"),
			ready: func(s *starvedLane) { codexPane(s, runtime.ProgramDone, nil) },
			change: func(s *starvedLane) {
				codexPane(s, runtime.ProgramDone, func(r *runtime.ProgramPaneRow) { r.Record = runtime.ProgramStatus{Nonce: r.Tap} })
			},
			reads: 2,
		},
		{
			name: "screen shows a turn", provider: "claude",
			ready:  func(s *starvedLane) { s.f.showTurn(false) },
			change: func(s *starvedLane) { s.f.showTurn(true) },
		},
		{
			name: "screen read fails", provider: "claude",
			ready: func(s *starvedLane) { s.f.sp.ScreenReadErrors = nil },
			change: func(s *starvedLane) {
				s.f.sp.ScreenReadErrors = map[string]error{s.f.name: runtime.ErrRuntimeUnavailable}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStarvedLane(t, tc.provider)
			if tc.city != nil {
				s.deps.cfg = tc.city
			}
			tc.ready(s)
			op := s.prompt(t, "k-send-look", "starved, then held at the send look")
			s.f.clk.Advance(10 * time.Minute)
			s.beforeLook = func(look int) {
				if look == 2 {
					tc.change(s)
				}
			}
			got := s.pass(t, op)
			if typed := s.f.nudges(); len(typed) != 0 {
				t.Fatalf("typed %+v though Deliver's look held; want nothing", typed)
			}
			if s.looks != 2 {
				t.Fatalf("%d starved looks; want the probe's and Deliver's", s.looks)
			}
			if n := s.f.sp.CountCalls("ProgramStatusHoldRead", s.f.name); n != tc.reads {
				t.Fatalf("%d record reads; want %d", n, tc.reads)
			}
			if got.Status != opjournal.StatusAccepted || got.Failures != 0 || got.MaybeDelivered != 0 || !strings.Contains(got.LastError, "before typing") {
				t.Fatalf("op %s, %d failures, maybe delivered %d, last error %q; want it handed back uncharged and not delivered, saying why",
					got.Status, got.Failures, got.MaybeDelivered, got.LastError)
			}
			s.beforeLook = nil
			tc.ready(s)
			s.f.clk.Advance(laneBusyMax)
			if got := s.pass(t, op); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded {
				t.Fatalf("once the look lets it go: op %s %+v; want closed succeeded", got.Status, got.Outcome)
			}
			if typed := s.f.nudges(); len(typed) != 1 {
				t.Fatalf("typed %+v; want the prompt once", typed)
			}
		})
	}
}

// TestHoldMidTurnWithoutRecord: a target is held without a record when its
// provider declares hold_mid_turn for its transport (the protocol declared,
// the transport not ACP) and the city switch is anything but on, no city
// included.
func TestHoldMidTurnWithoutRecord(t *testing.T) {
	declared, noHold := holdBinding(true), holdBinding(false)
	undeclared := declared
	undeclared.Protocol = config.ProgramStatusProtocolNone
	city := func(mode string) *config.City {
		return &config.City{Session: config.SessionConfig{ProgramStatus: mode}}
	}
	for _, tc := range []struct {
		name      string
		cfg       *config.City
		binding   *config.ProgramStatusBinding
		transport string
		want      bool
	}{
		{"off", city("off"), &declared, "", true},
		{"shadow", city("shadow"), &declared, "", true},
		{"unset", city(""), &declared, "", true},
		{"no city", nil, &declared, "", true},
		{"on reads the record", city("on"), &declared, "", false},
		{"no hold declared", city("off"), &noHold, "", false},
		{"protocol undeclared", city("off"), &undeclared, "", false},
		{"acp transport", city("off"), &declared, config.SessionTransportACP, false},
		{"no resolved provider", city("off"), nil, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := nudgeTarget{cfg: tc.cfg, transport: tc.transport}
			if tc.binding != nil {
				target.resolved = &config.ResolvedProvider{Name: "codex", ProgramStatus: *tc.binding}
			}
			if got := target.holdMidTurnWithoutRecord(); got != tc.want {
				t.Fatalf("holdMidTurnWithoutRecord() = %v, want %v", got, tc.want)
			}
		})
	}
	// The builtin profiles' declarations reach it through provider
	// resolution: codex declares the hold, claude does not.
	for provider, want := range map[string]bool{"codex": true, "claude": false} {
		catalog := map[string]config.ProviderSpec{provider: config.BuiltinProviderAlias(provider)}
		resolved, err := config.ResolveProvider(&config.Agent{Name: "worker", Provider: provider}, &config.Workspace{}, catalog,
			func(name string) (string, error) { return name, nil })
		if err != nil {
			t.Fatalf("resolving the builtin %s provider: %v", provider, err)
		}
		if got := (nudgeTarget{cfg: city("off"), resolved: resolved}).holdMidTurnWithoutRecord(); got != want {
			t.Errorf("builtin %s under off: holdMidTurnWithoutRecord() = %v, want %v", provider, got, want)
		}
	}
}

// TestNudgeTargetOfAProviderSessionDeclaresItsProvidersHold: a target whose
// names resolve to no agent, as a provider session's (API or dashboard
// kind=provider) do, takes the program-status declaration of the provider
// its starts resolve for it by name (its stored provider, then its
// template): the declaration its pane was armed with. Both holds on the
// starved bypass read it, the lane's without a record and the record's veto
// under on. Nothing else of that provider reaches the target, so its process
// names and provider name read as before. An agent session keeps its
// agent's declaration.
func TestNudgeTargetOfAProviderSessionDeclaresItsProvidersHold(t *testing.T) {
	provider := func(name, template string) session.Info {
		return session.Info{ID: "gc-1", SessionNameMetadata: "s-1", Template: template, Provider: name, SessionOrigin: "manual"}
	}
	for _, tc := range []struct {
		name string
		info session.Info
		city func(mode string) *config.City
		hold bool
	}{
		{"provider session", provider("codex", "codex"), codexCatalogCity, true},
		{"template names the provider", provider("", "codex"), codexCatalogCity, true},
		{"provider outside the catalog", provider("mystery", "mystery"), codexCatalogCity, false},
		{"agent session of another provider", session.Info{ID: "gc-1", SessionNameMetadata: "s-1", AgentName: "worker", Template: "worker", Provider: "codex"}, func(mode string) *config.City {
			cfg := codexAgentCity(mode)
			cfg.Agents[0].Provider = "claude"
			cfg.Providers["claude"] = config.ProviderSpec{Base: stringPtr("builtin:claude"), PathCheck: "true"}
			return cfg
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"off", "on"} {
				target := resolveNudgeTargetFromSessionInfo(t.TempDir(), tc.city(mode), tc.info)
				if got, want := target.holdMidTurnWithoutRecord(), tc.hold && mode == "off"; got != want {
					t.Errorf("under %s: holdMidTurnWithoutRecord() = %v, want %v", mode, got, want)
				}
				if got, want := target.programHoldDeclared(), tc.hold && mode == "on"; got != want {
					t.Errorf("under %s: programHoldDeclared() = %v, want %v", mode, got, want)
				}
			}
		})
	}
	// The pane's capture is armed with the same declaration (heal's want).
	cfg := codexCatalogCity("on")
	if arm := programCaptureWant(cfg, provider("codex", "codex")); !arm.HoldMidTurn {
		t.Fatalf("the provider session's capture want %+v; want the hold armed", arm)
	}
	target := resolveNudgeTargetFromSessionInfo(t.TempDir(), cfg, provider("codex", "codex"))
	if names, name := target.processNames(), target.providerName(); names != nil || name != "codex" {
		t.Fatalf("process names %v, provider name %q; want none and codex, as before the declaration", names, name)
	}
}

// TestLaneStarvedBoundaryReprobeKeepsThePassesContentAge: the host
// re-probes a busy pass's executor with its own view, which carries no
// content age; the re-probe judges starvation by the age the pass's probe
// read, so a never-quiet session's re-probe ends the busy wait once its
// content starves instead of re-probing until start_by.
func TestLaneStarvedBoundaryReprobeKeepsThePassesContentAge(t *testing.T) {
	s := newStarvedLane(t, "claude")
	op := s.prompt(t, "k-reprobe", "starved across a re-probe")
	s.f.sp.SetActivity(s.f.name, s.f.clk.Now())
	v, row := s.f.view(t)
	e := newConversationLaneExec(s.deps, row)
	quiescenceOnFixtureClock(s.f, e)
	if _, err := s.l.Pass(context.Background(), v, e); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if got := s.f.get(t, s.st, op.ID); got.Status == opjournal.StatusClosed {
		t.Fatalf("fresh op closed %+v on a busy pane", got.Outcome)
	}
	if !v.ContentSince.IsZero() {
		t.Fatalf("host view carries ContentSince %v; want zero", v.ContentSince)
	}
	s.f.clk.Advance(laneBoundaryStarvedAfter)
	s.f.sp.SetActivity(s.f.name, s.f.clk.Now())
	p, err := e.Reprobe(context.Background(), v)
	if err != nil {
		t.Fatalf("Reprobe: %v", err)
	}
	if p.Boundary != opjournal.TriYes || laneProbeStillBusy(p) {
		t.Fatalf("re-probe of starved content: boundary %v, still busy %v; want a boundary that ends the busy wait", p.Boundary, laneProbeStillBusy(p))
	}
}

// TestLaneStarvedBoundaryHoldsAnIndeterminateTurn: a family whose busy
// reading the runtime does not trust (runtime.ErrIdleIndeterminate) cannot
// show that no turn runs, so its starved content holds, as the legacy
// poller's does, and the host is not told of a failure on every look.
func TestLaneStarvedBoundaryHoldsAnIndeterminateTurn(t *testing.T) {
	s := newStarvedLane(t, "gemini")
	s.f.sp.ScreenReadErrors = map[string]error{s.f.name: runtime.ErrIdleIndeterminate}
	var noted []error
	s.deps.note = func(stage string, err error) {
		if stage == "boundary_starved" {
			noted = append(noted, err)
		}
	}
	op := s.prompt(t, "k-indeterminate", "starved on an unreadable turn")
	s.f.clk.Advance(10 * time.Minute)
	if got := s.pass(t, op); got.Status == opjournal.StatusClosed {
		t.Fatalf("op closed %+v with the turn indeterminate; want it held", got.Outcome)
	}
	if typed := s.f.nudges(); len(typed) != 0 || s.looks == 0 || len(noted) != 0 {
		t.Fatalf("typed %+v, looks %d, noted %v; want nothing typed after a look, nothing noted", typed, s.looks, noted)
	}
}
