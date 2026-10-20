package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
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
	// receipt is the last delivery's receipt.
	receipt runtime.DeliveryReceipt
	// stamped, when set, answers turn reads in place of history, given the
	// last delivery receipt's message stamp: a T3 turn the delivery started
	// starts at it.
	stamped func(delivered, readsSince int, stamp time.Time) (runtime.TurnHistory, error)
}

func (s *scriptedTurns) took(r runtime.DeliveryReceipt) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delivered++
	s.readsSent = 0
	s.receipt = r
}

func (s *scriptedTurns) NudgeFenced(ctx context.Context, name string, content []runtime.ContentBlock, n runtime.FencedNudge) error {
	var r runtime.DeliveryReceipt
	inner := n
	inner.Receipt = &r
	err := s.FullFake.NudgeFenced(ctx, name, content, inner)
	if err == nil {
		if n.Receipt != nil {
			*n.Receipt = r
		}
		s.took(r)
	}
	return err
}

func (s *scriptedTurns) NudgeKeyed(name string, content []runtime.ContentBlock, key string) (runtime.DeliveryReceipt, error) {
	r, err := s.FullFake.NudgeKeyed(name, content, key)
	if err == nil {
		s.took(r)
	}
	return r, err
}

func (s *scriptedTurns) TurnHistory(ctx context.Context, _ string) (runtime.TurnHistory, error) {
	if err := ctx.Err(); err != nil {
		return runtime.TurnHistory{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	history := s.history
	if s.stamped != nil {
		stamp := s.receipt.MessageAt
		history = func(delivered, readsSince int) (runtime.TurnHistory, error) {
			return s.stamped(delivered, readsSince, stamp)
		}
	}
	if history == nil {
		return runtime.TurnHistory{}, nil
	}
	h, err := history(s.delivered, s.readsSent)
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
	sp    runtime.Provider
	turns *scriptedTurns
	// transcript reads the scripted turns as a transcript source, on this
	// host's clock, instead of through the runtime.
	transcript bool
	mu         sync.Mutex
	elapsed    time.Duration // virtual time the link waits slept
}

// laneStubTurns is a worker.TurnHandle answering from caps and read.
type laneStubTurns struct {
	worker.TurnHandle
	caps    worker.TurnCapabilities
	capsErr error
	read    func(context.Context) (runtime.TurnHistory, error)
}

func (s laneStubTurns) TurnCapabilities(context.Context) (worker.TurnCapabilities, error) {
	return s.caps, s.capsErr
}

func (s laneStubTurns) CurrentTurn(ctx context.Context) (runtime.TurnHistory, error) {
	return s.read(ctx)
}

// laneTranscriptCaps are a transcript source's capabilities.
var laneTranscriptCaps = worker.TurnCapabilities{
	TurnCapabilities: runtime.TurnCapabilities{Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded},
	Source:           worker.TurnSourceTranscript,
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
	if f.transcript {
		e.turns = func() (worker.TurnHandle, error) {
			return laneStubTurns{caps: laneTranscriptCaps, read: func(ctx context.Context) (runtime.TurnHistory, error) {
				return f.turns.TurnHistory(ctx, f.name)
			}}, nil
		}
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

// startedAt is h with its current turn started at at.
func startedAt(h runtime.TurnHistory, at time.Time) runtime.TurnHistory {
	h.CurrentStart = at
	return h
}

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
// gives none after the link wait, and the wait never runs past it. A T3
// delivery links by its receipt's message stamp alone (row 3): a turn
// starting at the stamp is correlated, requested_at; a turn T3 restores as
// latest after the probe, or any turn whose start is not the stamp, never
// matches it, and the link is none after the full wait.
func TestLaneLinksScriptedTurns(t *testing.T) {
	readErr := errors.New("transcript unreadable")
	restored := runtime.EndedTurn{TurnID: t3Turn("t0"), StartedAt: laneT0.Add(-time.Minute), EndedAt: laneT0.Add(-30 * time.Second), Status: runtime.TurnStatusCompleted}
	for _, tc := range []struct {
		name       string
		profile    runtime.Profile
		opts       []laneFixtureOpt
		acpCaps    bool
		transcript bool
		history    func(delivered, readsSince int) (runtime.TurnHistory, error)
		stamped    func(delivered, readsSince int, stamp time.Time) (runtime.TurnHistory, error)
		turn       string
		link       opjournal.LinkConfidence
		basis      opjournal.LinkBasis
		fullWait   bool
		minReads   int
		noReads    bool
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
			name:    "correlated: the one new turn on this host's clock",
			profile: runtime.ProfileTmux, transcript: true,
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
			profile: runtime.ProfileTmux, transcript: true,
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
			profile: runtime.ProfileTmux, transcript: true,
			history: func(int, int) (runtime.TurnHistory, error) { return idleHistory, nil },
			link:    opjournal.LinkNone, basis: opjournal.BasisNoCandidate, fullWait: true, minReads: 6,
		},
		{
			name:    "none: every read after the effect failed",
			profile: runtime.ProfileTmux, transcript: true,
			history: func(delivered, _ int) (runtime.TurnHistory, error) {
				if delivered == 0 {
					return idleHistory, nil
				}
				return runtime.TurnHistory{}, readErr
			},
			link: opjournal.LinkNone, basis: opjournal.BasisReadFailed, fullWait: true, minReads: 6,
		},
		{
			name:    "correlated: T3's message stamp names the turn",
			profile: runtime.ProfileT3Bridge,
			stamped: func(delivered, _ int, stamp time.Time) (runtime.TurnHistory, error) {
				if delivered == 0 {
					return idleHistory, nil
				}
				return startedAt(running(t3Turn("t1")), stamp), nil
			},
			turn: t3Turn("t1"), link: opjournal.LinkCorrelated, basis: opjournal.BasisRequestedAt,
		},
		{
			name:    "none: T3 restores an ended turn as latest after the probe",
			profile: runtime.ProfileT3Bridge,
			history: func(delivered, _ int) (runtime.TurnHistory, error) {
				if delivered == 0 {
					return runtime.TurnHistory{}, nil // t0 ended and T3 cleared its latest turn
				}
				return runtime.TurnHistory{Recent: []runtime.EndedTurn{restored}}, nil // t0's diff completed: restored
			},
			link: opjournal.LinkNone, basis: opjournal.BasisNoCandidate, fullWait: true, minReads: 1,
		},
		{
			name:    "none: a T3 turn whose start is not the stamp",
			profile: runtime.ProfileT3Bridge,
			history: func(delivered, _ int) (runtime.TurnHistory, error) {
				if delivered == 0 {
					return idleHistory, nil
				}
				return running(t3Turn("t1")), nil
			},
			link: opjournal.LinkNone, basis: opjournal.BasisNoCandidate, fullWait: true, minReads: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTurnLaneFixture(t, tc.profile, tc.opts...)
			f.transcript = tc.transcript
			if tc.acpCaps {
				f.laneFixture.sp.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{f.name: {{
					Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkExact,
				}}}
			}
			f.turns.history, f.turns.stamped = tc.history, tc.stamped
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
			n := f.turns.reads()
			if tc.noReads && n != 0 {
				t.Fatalf("turn reads after the effect = %d, want none", n)
			}
			if !tc.noReads && n < max(tc.minReads, 1) {
				t.Fatalf("turn reads after the effect = %d, want at least %d", n, max(tc.minReads, 1))
			}
		})
	}
}

// TestLaneLinkWaitStopsWithTheEffect proves the link wait ends with the
// delivery's context: once it ends, no further read is made.
func TestLaneLinkWaitStopsWithTheEffect(t *testing.T) {
	f := newTurnLaneFixture(t, runtime.ProfileTmux)
	f.transcript = true
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
	f.turns.stamped = func(delivered, _ int, stamp time.Time) (runtime.TurnHistory, error) {
		switch {
		case failing:
			return runtime.TurnHistory{}, readErr
		case busy && delivered == 0:
			return running(t3Turn("human")), nil
		case delivered == 0:
			return idleHistory, nil
		default:
			return startedAt(running(t3Turn("t1")), stamp), nil
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
	r := f.get(t, st, op.ID).Outcome.Result
	if r.Turn != t3Turn("t1") || r.TurnLink != opjournal.LinkCorrelated || r.TurnBasis != opjournal.BasisRequestedAt {
		t.Fatalf("outcome = %+v; want linked correlated/requested_at to t1 by T3's message stamp", r)
	}
	if r.Delivery.MessageID == "" || r.Delivery.MessageAt.IsZero() {
		t.Fatalf("delivery = %+v; want T3's receipt recorded", r.Delivery)
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

// laneNotes records the notes an executor makes.
type laneNotes struct {
	mu    sync.Mutex
	notes []string
}

func (n *laneNotes) note(stage string, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.notes = append(n.notes, stage+": "+err.Error())
}

func (n *laneNotes) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.notes)
}

// TestLaneExecTurnTraitsFallbacks pins how a probe reads a session whose
// turn capabilities it cannot read: a session that is gone has no source; an
// unavailable read is an unavailable probe for a protocol-mode runtime,
// read again by the next probe, and no source, noted, for a keystroke-mode
// one; and a row no session handle is built from reads the runtime's turns,
// noted.
func TestLaneExecTurnTraitsFallbacks(t *testing.T) {
	unavailable := fmt.Errorf("loading the session: %w", runtime.ErrRuntimeUnavailable)
	t.Run("gone", func(t *testing.T) {
		f := newTurnLaneFixture(t, runtime.ProfileT3Bridge)
		e, v := f.exec(t, f.laneStore(t))
		e.turns = func() (worker.TurnHandle, error) { return nil, session.ErrSessionNotFound }
		p, err := e.Probe(context.Background(), v)
		if err != nil || !p.Available || e.turnHandle != nil || e.traits != (laneTurnTraits{}) {
			t.Fatalf("Probe = %+v, %v (traits %+v); want available with no turn source", p, err, e.traits)
		}
	})
	t.Run("unavailable, protocol mode", func(t *testing.T) {
		f := newTurnLaneFixture(t, runtime.ProfileT3Bridge)
		f.turns.history = func(int, int) (runtime.TurnHistory, error) { return idleHistory, nil }
		e, v := f.exec(t, f.laneStore(t))
		var notes laneNotes
		e.deps.note = notes.note
		resolve := e.turns
		e.turns = func() (worker.TurnHandle, error) {
			return laneStubTurns{capsErr: unavailable}, nil
		}
		p, err := e.Probe(context.Background(), v)
		if err != nil || p.Available || p.TurnLive != opjournal.TriUnknown || p.Fence != laneFence(v) {
			t.Fatalf("Probe = %+v, %v; want unavailable, no error", p, err)
		}
		e.turns = resolve
		if p, err = e.Probe(context.Background(), v); err != nil || !p.Available || !e.traits.protocol || p.Boundary != opjournal.TriYes {
			t.Fatalf("next Probe = %+v, %v (traits %+v); want the traits read again, at the boundary", p, err, e.traits)
		}
		if n := notes.all(); len(n) != 0 {
			t.Fatalf("notes = %q, want none: the probe answered unavailable", n)
		}
	})
	t.Run("unavailable, protocol mode, through the pass", func(t *testing.T) {
		f := newTurnLaneFixture(t, runtime.ProfileT3Bridge)
		f.turns.history = func(int, int) (runtime.TurnHistory, error) { return idleHistory, nil }
		st := f.laneStore(t)
		l := f.lane(t, st)
		f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi"))
		e, v := f.exec(t, st)
		e.turns = func() (worker.TurnHandle, error) { return laneStubTurns{capsErr: unavailable}, nil }
		res, err := l.Pass(context.Background(), v, e)
		if err != nil || res.Took != opjournal.TakeNone || !laneWaitsFor(res, opjournal.WaitUnavailable) {
			t.Fatalf("Pass = %+v, %v; want the op waiting unavailable, no error", res, err)
		}
	})
	t.Run("unavailable, keystroke mode", func(t *testing.T) {
		f := newTurnLaneFixture(t, runtime.ProfileTmux)
		e, v := f.exec(t, f.laneStore(t))
		var notes laneNotes
		e.deps.note = notes.note
		e.turns = func() (worker.TurnHandle, error) { return laneStubTurns{capsErr: unavailable}, nil }
		p, err := e.Probe(context.Background(), v)
		if err != nil || !p.Available || !p.Caps.Live || e.turnHandle != nil {
			t.Fatalf("Probe = %+v, %v; want available and live with no turn source", p, err)
		}
		if n := notes.all(); len(n) != 1 || !strings.HasPrefix(n[0], "turns: ") || !strings.Contains(n[0], "unreadable") {
			t.Fatalf("notes = %q, want the fallback noted once", n)
		}
	})
	t.Run("no session handle", func(t *testing.T) {
		f := newTurnLaneFixture(t, runtime.ProfileT3Bridge)
		f.turns.history = func(int, int) (runtime.TurnHistory, error) { return idleHistory, nil }
		e, v := f.exec(t, f.laneStore(t))
		var notes laneNotes
		e.deps.note = notes.note
		e.sessionTurns = func(string) (worker.TurnHandle, error) { return nil, errors.New("session row unreadable") }
		p, err := e.Probe(context.Background(), v)
		if err != nil || !p.Available || e.turnHandle == nil || !e.traits.protocol || p.TurnLive != opjournal.TriNo {
			t.Fatalf("Probe = %+v, %v (traits %+v); want the runtime's turns read", p, err, e.traits)
		}
		if n := notes.all(); len(n) != 1 || !strings.Contains(n[0], "session row unreadable") {
			t.Fatalf("notes = %q, want the fallback noted once", n)
		}
	})
}

// TestLaneExecProtocolLiveNeedsRecordedStarts pins Caps.Live for a
// protocol-mode runtime: live only when its turns, starts included, are
// reported, since a turn whose start is not recorded never reads as in
// progress and the boundary would always read yes.
func TestLaneExecProtocolLiveNeedsRecordedStarts(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps worker.TurnCapabilities
		live bool
	}{
		{name: "starts recorded", caps: worker.TurnCapabilities{TurnCapabilities: runtime.TurnCapabilities{Start: runtime.TurnStartRecorded}, Source: worker.TurnSourceProtocol}, live: true},
		{name: "starts unrecorded", caps: worker.TurnCapabilities{Source: worker.TurnSourceProtocol}},
		{name: "no source", caps: worker.TurnCapabilities{Source: worker.TurnSourceNone}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTurnLaneFixture(t, runtime.ProfileT3Bridge)
			e, v := f.exec(t, f.laneStore(t))
			e.turns = func() (worker.TurnHandle, error) {
				return laneStubTurns{caps: tc.caps, read: func(context.Context) (runtime.TurnHistory, error) { return idleHistory, nil }}, nil
			}
			p, err := e.Probe(context.Background(), v)
			if err != nil || p.Caps.Live != tc.live {
				t.Fatalf("Probe = caps %+v, %v; want Live %v", p.Caps, err, tc.live)
			}
		})
	}
}

// TestLaneLinkEvidenceSource pins which sources a delivery is linked by: a
// transcript (its cutoff on this host's clock) and ACP (serial) are; a T3
// source is only once the runtime stamped the message.
func TestLaneLinkEvidenceSource(t *testing.T) {
	recorded := runtime.TurnCapabilities{Start: runtime.TurnStartRecorded}
	stamped := laneT0.Add(time.Second)
	for _, tc := range []struct {
		name      string
		caps      worker.TurnCapabilities
		transport string
		messageAt time.Time
		want      bool
	}{
		{name: "transcript", caps: worker.TurnCapabilities{TurnCapabilities: recorded, Source: worker.TurnSourceTranscript}, want: true},
		{name: "acp", caps: worker.TurnCapabilities{TurnCapabilities: recorded, Source: worker.TurnSourceProtocol}, transport: "acp", want: true},
		{name: "t3, no stamp", caps: worker.TurnCapabilities{TurnCapabilities: recorded, Source: worker.TurnSourceProtocol}},
		{name: "t3, stamped", caps: worker.TurnCapabilities{TurnCapabilities: recorded, Source: worker.TurnSourceProtocol}, messageAt: stamped, want: true},
		{name: "starts unrecorded", caps: worker.TurnCapabilities{Source: worker.TurnSourceTranscript}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTurnLaneFixture(t, runtime.ProfileTmux)
			e, _ := f.exec(t, f.laneStore(t))
			e.turnHandle = laneStubTurns{read: func(context.Context) (runtime.TurnHistory, error) { return idleHistory, nil }}
			e.traits = laneTurnTraitsOf(tc.caps, tc.transport)
			e.lastCaps.LinkWait = time.Millisecond
			ev := e.linkEvidence(context.Background(), opjournal.DeliveryRecord{At: laneT0, MessageAt: tc.messageAt, Generation: 1, Mode: opjournal.LinkStarts, Ack: opjournal.AckDelivered})
			if ev.HasSource != tc.want {
				t.Fatalf("HasSource = %v, want %v", ev.HasSource, tc.want)
			}
		})
	}
}

// TestLaneLinkReadBoundedByTheWait proves no link read holds a delivery past
// the link wait: a read runs under a context that ends with the wait, and a
// read that ignores its context is abandoned when the wait passes.
func TestLaneLinkReadBoundedByTheWait(t *testing.T) {
	const wait = 50 * time.Millisecond
	for _, tc := range []struct {
		name       string
		honorsCtx  bool
		wantReason error
	}{
		{name: "the read honors its context", honorsCtx: true, wantReason: context.DeadlineExceeded},
		{name: "the read ignores its context", wantReason: runtime.ErrRuntimeUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTurnLaneFixture(t, runtime.ProfileTmux)
			e, _ := f.exec(t, f.laneStore(t))
			e.now, e.wait = time.Now, laneLinkWait
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			deadlines := make(chan bool, 8)
			e.turnHandle = laneStubTurns{read: func(ctx context.Context) (runtime.TurnHistory, error) {
				_, has := ctx.Deadline()
				deadlines <- has
				if tc.honorsCtx {
					<-ctx.Done()
					return runtime.TurnHistory{}, ctx.Err()
				}
				<-release
				return idleHistory, nil
			}}
			e.traits = laneTurnTraitsOf(laneTranscriptCaps, "")
			e.lastCaps.LinkWait = wait
			out := make(chan opjournal.TurnEvidence, 1)
			start := time.Now()
			go func() {
				out <- e.linkEvidence(context.Background(), opjournal.DeliveryRecord{At: laneT0, Generation: 1, Mode: opjournal.LinkStarts, Ack: opjournal.AckDelivered})
			}()
			select {
			case ev := <-out:
				if took := time.Since(start); took > 20*wait {
					t.Fatalf("link evidence took %s, want about the %s wait", took, wait)
				}
				if !errors.Is(ev.ReadErr, tc.wantReason) {
					t.Fatalf("ReadErr = %v, want %v", ev.ReadErr, tc.wantReason)
				}
			case <-time.After(laneWaitBudget):
				t.Fatalf("the link wait of %s held the delivery past %s", wait, laneWaitBudget)
			}
			if !<-deadlines {
				t.Fatal("the link read ran under a context with no deadline")
			}
		})
	}
}

// TestLaneNoteTurnReadOldestFirst pins one read's order in the evidence: its
// ended turns oldest start first (TurnHistory lists them newest first), then
// the running turn.
func TestLaneNoteTurnReadOldestFirst(t *testing.T) {
	older := runtime.EndedTurn{TurnID: "a", StartedAt: laneT0, EndedAt: laneT0.Add(time.Second), Status: runtime.TurnStatusCompleted}
	newer := runtime.EndedTurn{TurnID: "b", StartedAt: laneT0.Add(2 * time.Second), EndedAt: laneT0.Add(3 * time.Second), Status: runtime.TurnStatusCompleted}
	hist := runtime.TurnHistory{Current: runtime.TurnState{TurnID: "c"}, CurrentStart: laneT0.Add(4 * time.Second), Recent: []runtime.EndedTurn{newer, older}}
	ev := laneNoteTurnRead(opjournal.TurnEvidence{}, 1, hist, nil)
	var ids []string
	for _, s := range ev.Seen {
		ids = append(ids, s.ID)
	}
	if want := []string{"a", "b", "c"}; !slices.Equal(ids, want) {
		t.Fatalf("seen = %q, want %q", ids, want)
	}
}

// t3LaneFixture is a T3-profiled lane fixture whose turns start at the last
// delivery's message stamp, with the session idle until one is delivered.
func t3LaneFixture(t *testing.T) *turnLaneFixture {
	t.Helper()
	f := newTurnLaneFixture(t, runtime.ProfileT3Bridge)
	f.turns.stamped = func(delivered, _ int, stamp time.Time) (runtime.TurnHistory, error) {
		if delivered == 0 {
			return idleHistory, nil
		}
		return startedAt(running(t3Turn("t1")), stamp), nil
	}
	return f
}

// recordedPass is one pass whose nudge requests are recorded.
func (f *turnLaneFixture) recordedPass(t *testing.T, l *opjournal.ConversationLane, st *opjournal.Store) (opjournal.PassResult, []worker.NudgeRequest) {
	t.Helper()
	e, v := f.exec(t, st)
	var mu sync.Mutex
	var reqs []worker.NudgeRequest
	recordRequests(e, &mu, &reqs)
	res, err := l.Pass(context.Background(), v, e)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	return res, reqs
}

// pastRetry advances the fixture's clock past op's retry_at.
func (f *turnLaneFixture) pastRetry(t *testing.T, st *opjournal.Store, id string) {
	t.Helper()
	op := f.get(t, st, id)
	if d := op.RetryAt.Sub(f.clk.Now()); d > 0 {
		f.clk.Advance(d + time.Second)
	}
}

// laneCalls are the fake runtime's calls of method.
func (f *turnLaneFixture) laneCalls(method string) []runtime.Call {
	var out []runtime.Call
	for _, c := range f.laneFixture.sp.SnapshotCalls() {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// atLeastOnce makes a lane message at-least-once.
func atLeastOnce(r *opjournal.Request) { r.Policy.Guarantee = opjournal.GuaranteeAtLeastOnce }

// Kills a receipt asked of a leaf that does not deduplicate (M2 at the
// lane): a tmux delivery records no message id or stamp, and links by its
// history diff as before.
func TestLaneExecTmuxDeliveryRecordsNoMessageStamp(t *testing.T) {
	f := newTurnLaneFixture(t, runtime.ProfileTmux)
	f.transcript = true
	f.turns.history = func(delivered, _ int) (runtime.TurnHistory, error) {
		if delivered == 0 {
			return idleHistory, nil
		}
		return running(t3Turn("t1")), nil
	}
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi"))
	if res := f.pass(t, f.lane(t, st), st); res.Done != opjournal.DoneAccepted {
		t.Fatalf("Pass = %+v; want accepted", res)
	}
	r := f.get(t, st, op.ID).Outcome.Result
	if r.Delivery.MessageID != "" || !r.Delivery.MessageAt.IsZero() {
		t.Fatalf("delivery = %+v; want no message id or stamp from tmux", r.Delivery)
	}
	if r.TurnLink != opjournal.LinkCorrelated || r.TurnBasis != opjournal.BasisHistoryDiff {
		t.Fatalf("link = %s/%s; want correlated/history_diff", r.TurnLink, r.TurnBasis)
	}
}

// Pins T2's key rule end to end on T3 (M31): after an ambiguous failure the
// re-take looks the earlier delivery up under the same key and, finding none,
// sends once under that key, linked by its stamp.
func TestLaneT3AmbiguousFailureRedeliversUnderTheSameKey(t *testing.T) {
	f := t3LaneFixture(t)
	f.laneFixture.sp.NudgeErrors = map[string]error{f.name: fmt.Errorf("t3 took the turn without answering: %w", runtime.ErrNudgeDeliveryUnconfirmed)}
	st := f.laneStore(t)
	l := f.lane(t, st)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "deploy it", atLeastOnce))
	res, first := f.recordedPass(t, l, st)
	if res.Done != opjournal.DoneReleased || len(first) != 1 || first[0].Redelivery {
		t.Fatalf("pass 1 = %+v with requests %+v; want released after one first delivery", res, first)
	}
	if got := f.get(t, st, op.ID); got.MaybeDelivered != 1 {
		t.Fatalf("op after pass 1 = %+v; want maybe_delivered", got)
	}
	f.laneFixture.sp.NudgeErrors = nil
	f.pastRetry(t, st, op.ID)

	res, second := f.recordedPass(t, l, st)
	if res.Done != opjournal.DoneAccepted || len(second) != 1 || !second[0].Redelivery || second[0].DeliveryKey != first[0].DeliveryKey {
		t.Fatalf("pass 2 = %+v with requests %+v; want one redelivery under %q", res, second, first[0].DeliveryKey)
	}
	if looks := f.laneCalls("LookupDelivery"); len(looks) != 1 || looks[0].DeliveryKey != first[0].DeliveryKey {
		t.Fatalf("lookups = %+v; want one under the pass-1 key", looks)
	}
	nudges := f.nudges()
	if len(nudges) != 2 || nudges[1].DeliveryKey != first[0].DeliveryKey {
		t.Fatalf("runtime nudges = %+v; want the redelivery under the pass-1 key", nudges)
	}
	if strings.Contains(nudges[1].Message, opjournal.RedeliveryNote) {
		t.Fatalf("redelivered text %q carries the D-NOTE line on a deduplicating runtime", nudges[1].Message)
	}
	r := f.get(t, st, op.ID).Outcome.Result
	if r.Via != laneViaLive || r.TurnLink != opjournal.LinkCorrelated || r.TurnBasis != opjournal.BasisRequestedAt {
		t.Fatalf("outcome = %+v; want via live, correlated/requested_at", r)
	}
}

// Kills link evidence gathered for a replay (M32) and a last-nudge stamp
// written for one (M33): a re-take whose delivery T3 already holds sends
// nothing and closes succeeded via replayed with the held stamp.
func TestLaneT3RedeliveryHeldIsReplayed(t *testing.T) {
	f := t3LaneFixture(t)
	f.laneFixture.sp.NudgeErrors = map[string]error{f.name: runtime.ErrNudgeDeliveryUnconfirmed}
	st := f.laneStore(t)
	l := f.lane(t, st)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "deploy it", atLeastOnce))
	if res, _ := f.recordedPass(t, l, st); res.Done != opjournal.DoneReleased {
		t.Fatalf("pass 1 = %+v; want released", res)
	}
	key := opjournal.RuntimeDeliveryKey(f.get(t, st, op.ID).DeliveryKey)
	held := runtime.DeliveryReceipt{MessageID: "t3-message-1", MessageAt: laneT0.Add(-time.Minute).Add(123 * time.Millisecond)}
	f.laneFixture.sp.KeyedDeliveries = map[string]map[string]runtime.DeliveryReceipt{f.name: {key: held}}
	f.laneFixture.sp.NudgeErrors = nil
	f.pastRetry(t, st, op.ID)
	before := len(f.nudges())
	waited := f.waited()

	if res, _ := f.recordedPass(t, l, st); res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass 2 = %+v; want accepted", res)
	}
	if n := len(f.nudges()); n != before {
		t.Fatalf("runtime nudges = %d after the replay, want still %d", n, before)
	}
	got := f.get(t, st, op.ID)
	r := got.Outcome.Result
	if got.Outcome.Arm != opjournal.ArmSucceeded || r.Via != laneViaReplayed || r.Delivery.MessageID != held.MessageID || !r.Delivery.MessageAt.Equal(held.MessageAt) {
		t.Fatalf("outcome = %s %+v; want succeeded via replayed with the held receipt %+v", got.Outcome.Arm, r, held)
	}
	if r.TurnLink != opjournal.LinkNone || r.TurnBasis != opjournal.BasisNoCandidate {
		t.Fatalf("link = %s/%s; want none/no_candidate", r.TurnLink, r.TurnBasis)
	}
	if f.waited() != waited || f.turns.reads() != 0 {
		t.Fatalf("link wait = %s, reads after an effect = %d; want no link wait and no reads for a replay", f.waited()-waited, f.turns.reads())
	}
	row, err := f.store.Get(f.id)
	if err != nil || row.Metadata[session.MetadataLastNudgeDeliveredAt] != "" {
		t.Fatalf("session row = %v, %v; want no last-nudge stamp for a replay", row.Metadata, err)
	}
}

// Kills a refused key reused (M34): after T3 rejects a delivery the next
// Take mints a new key for the same head op and does not look the refused
// one up.
func TestLaneT3RejectedKeyIsBumped(t *testing.T) {
	f := t3LaneFixture(t)
	f.laneFixture.sp.NudgeErrors = map[string]error{f.name: fmt.Errorf("t3 refused the turn: %w", runtime.ErrNudgeRejected)}
	st := f.laneStore(t)
	l := f.lane(t, st)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "deploy it", atLeastOnce))
	res, first := f.recordedPass(t, l, st)
	if res.Done != opjournal.DoneReleased || len(first) != 1 {
		t.Fatalf("pass 1 = %+v with requests %+v; want released", res, first)
	}
	if got := f.get(t, st, op.ID); got.Failures != 1 {
		t.Fatalf("op after a rejection = %+v; want one failure counted", got)
	}
	f.laneFixture.sp.NudgeErrors = nil
	f.pastRetry(t, st, op.ID)

	res, second := f.recordedPass(t, l, st)
	if res.Done != opjournal.DoneAccepted || len(second) != 1 {
		t.Fatalf("pass 2 = %+v with requests %+v; want accepted", res, second)
	}
	prefix := opjournal.RuntimeDeliveryKey(op.ID) + ":"
	if second[0].DeliveryKey == first[0].DeliveryKey || !strings.HasPrefix(second[0].DeliveryKey, prefix) || !strings.HasPrefix(first[0].DeliveryKey, prefix) {
		t.Fatalf("keys = %q then %q; want a new key for head %s", first[0].DeliveryKey, second[0].DeliveryKey, op.ID)
	}
	if second[0].Redelivery || len(f.laneCalls("LookupDelivery")) != 0 {
		t.Fatalf("pass 2 request %+v, lookups %+v; want a fresh delivery that looks nothing up", second[0], f.laneCalls("LookupDelivery"))
	}
}
