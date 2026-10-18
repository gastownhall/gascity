package main

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// scriptedTurns is a runtime backend whose turns are scripted against the
// deliveries it took: history answers each turn read from how many nudges
// were delivered and how many reads followed the last one.
type scriptedTurns struct {
	runtime.FullFake
	mu        sync.Mutex
	delivered int
	readsSent int // reads since the last delivery
	history   func(delivered, readsSince int) (runtime.TurnHistory, error)
}

func (s *scriptedTurns) took() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delivered++
	s.readsSent = 0
}

func (s *scriptedTurns) NudgeFenced(ctx context.Context, name string, content []runtime.ContentBlock, n runtime.FencedNudge) error {
	err := s.FullFake.NudgeFenced(ctx, name, content, n)
	if err == nil {
		s.took()
	}
	return err
}

func (s *scriptedTurns) NudgeKeyed(name string, content []runtime.ContentBlock, key string) error {
	err := s.FullFake.NudgeKeyed(name, content, key)
	if err == nil {
		s.took()
	}
	return err
}

func (s *scriptedTurns) TurnHistory(ctx context.Context, _ string) (runtime.TurnHistory, error) {
	if err := ctx.Err(); err != nil {
		return runtime.TurnHistory{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.history == nil {
		return runtime.TurnHistory{}, nil
	}
	h, err := s.history(s.delivered, s.readsSent)
	if s.delivered > 0 {
		s.readsSent++
	}
	return h, err
}

func (s *scriptedTurns) reads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readsSent
}

// turnLaneFixture is a lane fixture over a profiled runtime with scripted
// turns, and a virtual clock for the link wait.
type turnLaneFixture struct {
	*laneFixture
	sp      runtime.Provider
	turns   *scriptedTurns
	mu      sync.Mutex
	elapsed time.Duration // virtual time the link waits slept
}

func newTurnLaneFixture(t *testing.T, p runtime.Profile, opts ...laneFixtureOpt) *turnLaneFixture {
	t.Helper()
	f := newLaneFixture(t, opts...)
	turns := &scriptedTurns{FullFake: runtime.FullFake{Fake: f.sp}}
	return &turnLaneFixture{laneFixture: f, sp: runtime.NewFakeProfile(p, turns), turns: turns}
}

func (f *turnLaneFixture) deps(st *opjournal.Store) laneExecDeps {
	d := f.laneFixture.deps(st)
	d.sp = f.sp
	return d
}

// exec is the live executor over the profiled runtime, its link wait on the
// fixture's virtual clock.
func (f *turnLaneFixture) exec(t *testing.T, st *opjournal.Store) (*conversationLaneExec, opjournal.ConversationView) {
	t.Helper()
	v, info := f.view(t)
	e := newConversationLaneExec(f.deps(st), info)
	start := time.Now()
	e.now = func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return start.Add(f.elapsed)
	}
	e.wait = func(ctx context.Context, d time.Duration) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.elapsed += max(d, 0)
		return ctx.Err() == nil
	}
	return e, v
}

func (f *turnLaneFixture) pass(t *testing.T, l *opjournal.ConversationLane, st *opjournal.Store) opjournal.PassResult {
	t.Helper()
	e, v := f.exec(t, st)
	res, err := l.Pass(context.Background(), v, e)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	return res
}

func (f *turnLaneFixture) waited() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.elapsed
}

// t3Turn is a T3-shaped turn id.
func t3Turn(n string) string { return "thread-1/" + n }

var (
	turnStartAt = laneT0.Add(-time.Hour)
	// endedOld is the turn that ended before the probe.
	endedOld = runtime.EndedTurn{TurnID: t3Turn("t0"), StartedAt: turnStartAt, EndedAt: turnStartAt.Add(time.Minute), Status: runtime.TurnStatusCompleted}
	// idleHistory is a session at rest after endedOld.
	idleHistory = runtime.TurnHistory{Recent: []runtime.EndedTurn{endedOld}}
)

// running is a history with turn id in progress after endedOld, plus the
// turns in also, newest first.
func running(id string, also ...runtime.EndedTurn) runtime.TurnHistory {
	return runtime.TurnHistory{
		Current:      runtime.TurnState{TurnID: id},
		CurrentStart: laneT0.Add(time.Second),
		Recent:       append(slices.Clone(also), endedOld),
	}
}

// TestLaneLinksScriptedTurns pins DES-O1 §7.7's links: on a runtime with
// scripted turns every delivered op closes with the scripted turn and the
// expected confidence, a competing turn gives ambiguous, a silent runtime
// gives none after the link wait, and the wait never runs past it.
func TestLaneLinksScriptedTurns(t *testing.T) {
	readErr := errors.New("snapshot unreadable")
	for _, tc := range []struct {
		name      string
		profile   runtime.Profile
		opts      []laneFixtureOpt
		acpCaps   bool
		history   func(delivered, readsSince int) (runtime.TurnHistory, error)
		turn      string
		link      opjournal.LinkConfidence
		basis     opjournal.LinkBasis
		fullWait  bool
		minReads  int
		unchecked bool
	}{
		{
			name:    "exact: the one new turn on a serial source",
			profile: runtime.ProfileACP, opts: []laneFixtureOpt{laneTransport("acp")}, acpCaps: true,
			history: func(delivered, _ int) (runtime.TurnHistory, error) {
				if delivered == 0 {
					return idleHistory, nil
				}
				return running(t3Turn("t1")), nil
			},
			turn: t3Turn("t1"), link: opjournal.LinkExact, basis: opjournal.BasisACPSerial,
		},
		{
			name:    "correlated: the one new turn",
			profile: runtime.ProfileT3Bridge,
			history: func(delivered, readsSince int) (runtime.TurnHistory, error) {
				if delivered == 0 || readsSince < 2 {
					return idleHistory, nil
				}
				return running(t3Turn("t1")), nil
			},
			turn: t3Turn("t1"), link: opjournal.LinkCorrelated, basis: opjournal.BasisHistoryDiff, minReads: 3,
		},
		{
			name:    "ambiguous: a competing turn",
			profile: runtime.ProfileT3Bridge,
			history: func(delivered, _ int) (runtime.TurnHistory, error) {
				if delivered == 0 {
					return idleHistory, nil
				}
				human := runtime.EndedTurn{TurnID: t3Turn("h1"), StartedAt: laneT0, EndedAt: laneT0.Add(time.Millisecond), Status: runtime.TurnStatusCompleted}
				return running(t3Turn("t1"), human), nil
			},
			turn: t3Turn("h1"), link: opjournal.LinkAmbiguous, basis: opjournal.BasisCompetingTurn,
		},
		{
			name:    "none: a silent runtime",
			profile: runtime.ProfileT3Bridge,
			history: func(int, int) (runtime.TurnHistory, error) { return idleHistory, nil },
			link:    opjournal.LinkNone, basis: opjournal.BasisNoCandidate, fullWait: true, minReads: 6,
		},
		{
			name:    "none: every read after the effect failed",
			profile: runtime.ProfileT3Bridge,
			history: func(delivered, _ int) (runtime.TurnHistory, error) {
				if delivered == 0 {
					return idleHistory, nil
				}
				return runtime.TurnHistory{}, readErr
			},
			link: opjournal.LinkNone, basis: opjournal.BasisReadFailed, fullWait: true, minReads: 6,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTurnLaneFixture(t, tc.profile, tc.opts...)
			if tc.acpCaps {
				f.laneFixture.sp.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{f.name: {{
					Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkExact,
				}}}
			}
			f.turns.history = tc.history
			st := f.laneStore(t)
			op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "please run the tests"))
			if res := f.pass(t, f.lane(t, st), st); res.Done != opjournal.DoneAccepted {
				t.Fatalf("Pass = %+v; want accepted", res)
			}
			got := f.get(t, st, op.ID)
			r := got.Outcome.Result
			if got.Outcome.Arm != opjournal.ArmSucceeded || r.Turn != tc.turn || r.TurnLink != tc.link || r.TurnBasis != tc.basis {
				t.Fatalf("outcome = %s turn %q link %s/%s; want succeeded turn %q link %s/%s",
					got.Outcome.Arm, r.Turn, r.TurnLink, r.TurnBasis, tc.turn, tc.link, tc.basis)
			}
			if w := f.waited(); w > opjournal.DefaultLinkWait || tc.fullWait && w != opjournal.DefaultLinkWait {
				t.Fatalf("link wait = %s; want at most %s (all of it: %v)", w, opjournal.DefaultLinkWait, tc.fullWait)
			}
			if !tc.fullWait && f.waited() == opjournal.DefaultLinkWait {
				t.Fatal("a decided link waited out the whole link wait")
			}
			if n := f.turns.reads(); n < max(tc.minReads, 1) {
				t.Fatalf("turn reads after the effect = %d, want at least %d", n, max(tc.minReads, 1))
			}
		})
	}
}

// TestLaneLinkWaitStopsWithTheEffect proves the link wait ends with the
// delivery's context: once it ends, no further read is made.
func TestLaneLinkWaitStopsWithTheEffect(t *testing.T) {
	f := newTurnLaneFixture(t, runtime.ProfileT3Bridge)
	var reads int
	f.turns.history = func(int, int) (runtime.TurnHistory, error) {
		reads++
		return idleHistory, nil
	}
	e, _ := f.exec(t, f.laneStore(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waits := 0
	e.wait = func(context.Context, time.Duration) bool {
		waits++
		cancel()
		return false
	}
	if err := e.readTurnTraits(ctx); err != nil {
		t.Fatalf("readTurnTraits: %v", err)
	}
	e.lastCaps.LinkWait = opjournal.DefaultLinkWait
	e.lastTurn = laneTurnLookOf(idleHistory, nil)
	ev := e.linkEvidence(ctx, opjournal.DeliveryRecord{At: laneT0, Generation: 1, Mode: opjournal.LinkStarts, Ack: opjournal.AckDelivered})
	if !ev.HasSource || !ev.FirstRead.Done || reads != 1 || waits != 1 {
		t.Fatalf("evidence = %+v after %d reads and %d waits; want one read, then the wait ended", ev, reads, waits)
	}
}

// TestLaneExecProtocolDeliversOnlyAtItsBoundary pins DES-T3I §14 item 6: a
// protocol-mode runtime is live, its boundary is CurrentTurn showing no turn
// in progress whatever the quiescence rule says, and nothing is typed while
// a turn runs or the read says nothing.
func TestLaneExecProtocolDeliversOnlyAtItsBoundary(t *testing.T) {
	readErr := errors.New("snapshot unreadable")
	busy := true
	failing := false
	f := newTurnLaneFixture(t, runtime.ProfileT3Bridge)
	f.turns.history = func(delivered, _ int) (runtime.TurnHistory, error) {
		switch {
		case failing:
			return runtime.TurnHistory{}, readErr
		case busy && delivered == 0:
			return running(t3Turn("human")), nil
		case delivered == 0:
			return idleHistory, nil
		default:
			return running(t3Turn("t1")), nil
		}
	}
	// The quiescence rule would call the session busy: it was active just
	// now. A protocol runtime never reads it.
	f.laneFixture.sp.SetActivity(f.name, time.Now())
	st := f.laneStore(t)
	l := f.lane(t, st)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi"))

	res := f.pass(t, l, st)
	if res.Took != opjournal.TakeNone || !res.Busy || !laneWaitsFor(res, opjournal.WaitBusy) {
		t.Fatalf("pass with a turn in progress = %+v; want nothing taken, busy", res)
	}
	e, v := f.exec(t, st)
	if p, err := e.Reprobe(context.Background(), v); err != nil || !laneProbeStillBusy(p) || p.Turn != t3Turn("human") {
		t.Fatalf("re-probe = %+v, %v; want still busy with the human's turn", p, err)
	}

	busy, failing = false, true
	res = f.pass(t, l, st)
	if res.Took != opjournal.TakeNone || !laneWaitsFor(res, opjournal.WaitBoundaryUnknown) {
		t.Fatalf("pass with an unreadable turn = %+v; want nothing taken, boundary_unknown", res)
	}
	if len(f.nudges()) != 0 {
		t.Fatalf("runtime nudges = %+v before the boundary, want none", f.nudges())
	}

	failing = false
	if res = f.pass(t, l, st); res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass at the boundary = %+v; want accepted", res)
	}
	if calls := f.nudges(); len(calls) != 1 || calls[0].Message != "hi" {
		t.Fatalf("runtime nudges = %+v, want the prompt once", calls)
	}
	if got := f.get(t, st, op.ID); got.Outcome.Result.Turn != t3Turn("t1") || got.Outcome.Result.TurnLink != opjournal.LinkCorrelated {
		t.Fatalf("outcome = %+v; want linked to t1, correlated", got.Outcome)
	}
}

// TestLaneExecProbeReportsTurns pins Probe.Turn, TurnLive and KnownTurns from
// CurrentTurn, unknown on a failed read and on a runtime with no turn
// source.
func TestLaneExecProbeReportsTurns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile runtime.Profile
		opts    []laneFixtureOpt
		history runtime.TurnHistory
		err     error
		live    opjournal.Tri
		turn    string
		known   []string
	}{
		{name: "idle", profile: runtime.ProfileT3Bridge, history: idleHistory, live: opjournal.TriNo, known: []string{t3Turn("t0")}},
		{name: "running", profile: runtime.ProfileT3Bridge, history: running(t3Turn("t1")), live: opjournal.TriYes, turn: t3Turn("t1"), known: []string{t3Turn("t1"), t3Turn("t0")}},
		{name: "read failed", profile: runtime.ProfileT3Bridge, err: runtime.ErrRuntimeUnavailable, live: opjournal.TriUnknown},
		{name: "no source", profile: runtime.ProfileTmux, history: running(t3Turn("t1")), live: opjournal.TriUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTurnLaneFixture(t, tc.profile, tc.opts...)
			f.turns.history = func(int, int) (runtime.TurnHistory, error) { return tc.history, tc.err }
			e, v := f.exec(t, f.laneStore(t))
			p, err := e.Probe(context.Background(), v)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if p.TurnLive != tc.live || p.Turn != tc.turn || !slices.Equal(p.KnownTurns, tc.known) {
				t.Fatalf("probe turn = %v %q known %q; want %v %q known %q", p.TurnLive, p.Turn, p.KnownTurns, tc.live, tc.turn, tc.known)
			}
		})
	}
}

// TestLaneExecKeystrokeReprobeReadsNoTurns proves a keystroke-mode runtime's
// busy re-probe reads no turns: its boundary is the quiescence rule.
func TestLaneExecKeystrokeReprobeReadsNoTurns(t *testing.T) {
	f := newTurnLaneFixture(t, runtime.ProfileACP, laneTransport("acp"))
	f.laneFixture.sp.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{f.name: {{
		Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkExact,
	}}}
	var reads int
	f.turns.history = func(int, int) (runtime.TurnHistory, error) {
		reads++
		return idleHistory, nil
	}
	e, v := f.exec(t, f.laneStore(t))
	if _, err := e.Probe(context.Background(), v); err != nil || reads != 1 {
		t.Fatalf("Probe = %v with %d turn reads; want one", err, reads)
	}
	if _, err := e.Reprobe(context.Background(), v); err != nil || reads != 1 {
		t.Fatalf("Reprobe = %v; turn reads %d, want none more", err, reads)
	}
}

// TestLaneExecDedupThroughTheWorkerBoundary pins Caps.Dedup: true for a
// runtime that deduplicates keyed deliveries (t3bridge), false for one that
// does not.
func TestLaneExecDedupThroughTheWorkerBoundary(t *testing.T) {
	for _, tc := range []struct {
		profile runtime.Profile
		want    bool
	}{
		{profile: runtime.ProfileT3Bridge, want: true},
		{profile: runtime.ProfileTmux},
	} {
		t.Run(tc.profile.String(), func(t *testing.T) {
			f := newTurnLaneFixture(t, tc.profile)
			e, v := f.exec(t, f.laneStore(t))
			p, err := e.Probe(context.Background(), v)
			if err != nil || p.Caps.Dedup != tc.want {
				t.Fatalf("Probe = caps %+v, %v; want Dedup %v", p.Caps, err, tc.want)
			}
		})
	}
}

// TestLaneTurnTraitsOf pins the evidence traits each turn source gives.
func TestLaneTurnTraitsOf(t *testing.T) {
	recorded := runtime.TurnCapabilities{Start: runtime.TurnStartRecorded}
	for _, tc := range []struct {
		name      string
		caps      worker.TurnCapabilities
		transport string
		want      laneTurnTraits
	}{
		{name: "none", caps: worker.TurnCapabilities{Source: worker.TurnSourceNone}},
		{
			name: "transcript", caps: worker.TurnCapabilities{TurnCapabilities: recorded, Source: worker.TurnSourceTranscript},
			want: laneTurnTraits{readable: true, starts: true, sameClock: true},
		},
		{
			name: "t3", caps: worker.TurnCapabilities{TurnCapabilities: recorded, Source: worker.TurnSourceProtocol},
			want: laneTurnTraits{readable: true, protocol: true, starts: true},
		},
		{
			name: "acp", caps: worker.TurnCapabilities{TurnCapabilities: recorded, Source: worker.TurnSourceProtocol}, transport: "acp",
			want: laneTurnTraits{readable: true, protocol: true, starts: true, sameClock: true, serial: true},
		},
		{
			name: "protocol, starts unrecorded", caps: worker.TurnCapabilities{Source: worker.TurnSourceProtocol},
			want: laneTurnTraits{readable: true, protocol: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := laneTurnTraitsOf(tc.caps, tc.transport); got != tc.want {
				t.Fatalf("traits = %+v, want %+v", got, tc.want)
			}
		})
	}
}
