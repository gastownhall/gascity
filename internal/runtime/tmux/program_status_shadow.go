package tmux

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// Shadow mode at the tmux leaf (design §9). With the switch at shadow and a
// comparison sink configured, each leaf decision point records today's
// reading beside what the program's own record would decide. Nothing here
// feeds a decision back: every point decides exactly as it does with the
// switch off. A reading a point needs before it acts (a baseline taken before
// gc types) is one tmux read on the decision path; everything after the
// decision (the record read that judges it, any follow-up that times the
// program's own signal, the hand-off to the sink) runs on a bounded
// background goroutine, and a point whose work finds no free slot is
// dropped and counted, never waited for.
//
// A timed point follows its record past the decision only in a process that
// lives long enough (Config.ComparisonFollowUps), and only one follow-up per
// session and point is outstanding at a time: a decision that finds one
// running records its decision-time verdict untimed instead, so a point that
// repeats while its session stays busy can neither flood the log with
// overlapping samples nor fill the slots.

const (
	// shadowSlots bounds the background shadow work one Tmux runs at once.
	shadowSlots = 64
	// shadowIdentityTTL is how long a session's family and bead id are
	// reused before the session environment is read again.
	shadowIdentityTTL = 10 * time.Minute
	// shadowInterruptWindow is how long after an interrupt a settle wait is
	// read as that interrupt's settle.
	shadowInterruptWindow = 30 * time.Second
	// shadowReadyFollowLimit bounds how long readiness waits for the
	// program's first valid report after the scrape decided.
	shadowReadyFollowLimit = 60 * time.Second
	// shadowConfirmFollowLimit bounds how long a submit or settle waits for
	// the program's evidence after the scrape decided.
	shadowConfirmFollowLimit = 30 * time.Second
)

// shadowFollowInterval is the pause between two follow-up reads at elapsed
// time into the follow-up: 500 ms for the first 30 s, 2 s after. A var so
// tests take no wall time.
var shadowFollowInterval = func(elapsed time.Duration) time.Duration {
	if elapsed < 30*time.Second {
		return 500 * time.Millisecond
	}
	return 2 * time.Second
}

// programShadow is a Tmux's shadow state: soft, per process, rebuilt on
// demand.
type programShadow struct {
	// slots bounds the background work (shadowSlots).
	slotsOnce sync.Once
	slots     chan struct{}
	// dropped counts comparisons dropped for want of a slot since the last
	// one handed to the sink.
	dropped atomic.Int64

	mu sync.Mutex
	// identity memoizes each session's family and bead id.
	identity map[string]shadowIdentity
	// interrupts holds each session's last interrupt baseline, for the
	// settle wait that follows it.
	interrupts map[string]shadowInterrupt
	// taps returns the state cache's process-table account of capture
	// readers without refreshing it; nil reads tap liveness as unknown.
	taps func() programTaps
	// following holds the session points with a follow-up outstanding.
	following map[shadowFollowKey]struct{}
}

// shadowFollowKey is one session's timed decision point.
type shadowFollowKey struct {
	session string
	point   runtime.ProgramStatusPoint
}

// shadowIdentity is a session's family and bead id as its environment named
// them at read.
type shadowIdentity struct {
	family, id string
	read       time.Time
}

// shadowInterrupt is the record's position when gc interrupted a session.
type shadowInterrupt struct {
	baseline runtime.ProgramBaseline
	at       time.Time
}

// shadowSink returns the sink comparisons go to, nil unless the switch is
// shadow and a sink is configured.
func (t *Tmux) shadowSink() runtime.ProgramStatusComparisonSink {
	if t.programStatusMode() != runtime.ProgramStatusShadow {
		return nil
	}
	return t.cfg.ComparisonSink
}

// shadowing reports whether the leaf's decision points record comparisons.
func (t *Tmux) shadowing() bool { return t.shadowSink() != nil }

// ProgramStatusShadowSink implements [runtime.ProgramStatusShadowProvider]:
// the comparison sink, nil unless the switch is shadow. A comparison that
// arrives without its session's family or bead id has them filled from the
// session's environment on a shadow goroutine, never on the caller's path.
func (p *Provider) ProgramStatusShadowSink() runtime.ProgramStatusComparisonSink {
	sink := p.tm.shadowSink()
	if sink == nil {
		return nil
	}
	return func(c runtime.ProgramStatusComparison) {
		if c.Family != "" && c.SessionID != "" {
			sink(c)
			return
		}
		p.tm.shadowGo(func() {
			family, id := p.tm.shadowIdentityOf(c.Session)
			if c.Family == "" {
				c.Family = family
			}
			if c.SessionID == "" {
				c.SessionID = id
			}
			c.Dropped += int(p.tm.shadow.dropped.Swap(0))
			sink(c)
		})
	}
}

var _ runtime.ProgramStatusShadowProvider = (*Provider)(nil)

// shadowGo runs fn on a background goroutine when a slot is free, and
// otherwise drops it, counting the drop against the next comparison. The
// process is told of the work (Config.ComparisonWork) so its exit can wait
// for it. A panic in fn is contained (a supervisor process hosts several
// cities) and counted as a drop.
func (t *Tmux) shadowGo(fn func()) {
	sh := &t.shadow
	sh.slotsOnce.Do(func() { sh.slots = make(chan struct{}, shadowSlots) })
	select {
	case sh.slots <- struct{}{}:
	default:
		sh.dropped.Add(1)
		return
	}
	done := func() {}
	if t.cfg.ComparisonWork != nil {
		done = t.cfg.ComparisonWork()
	}
	go func() {
		defer done()
		defer func() { <-sh.slots }()
		defer func() {
			if recover() != nil {
				sh.dropped.Add(1)
			}
		}()
		fn()
	}()
}

// shadowFollowsUp reports whether the process follows timed comparisons
// past their decision (Config.ComparisonFollowUps).
func (t *Tmux) shadowFollowsUp() bool {
	return t.cfg.ComparisonFollowUps != nil && t.cfg.ComparisonFollowUps()
}

// claimShadowFollow claims session's point for one follow-up and returns
// the call that releases it, or false when the process does not follow up
// or a follow-up of that session and point is already outstanding.
func (t *Tmux) claimShadowFollow(session string, point runtime.ProgramStatusPoint) (func(), bool) {
	if !t.shadowFollowsUp() {
		return nil, false
	}
	sh := &t.shadow
	key := shadowFollowKey{session: session, point: point}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if _, busy := sh.following[key]; busy {
		return nil, false
	}
	if sh.following == nil {
		sh.following = make(map[shadowFollowKey]struct{})
	}
	sh.following[key] = struct{}{}
	return func() {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		delete(sh.following, key)
	}, true
}

// programFreshFormat reads every pane of a session for a fresh judgement:
// pane_dead, then programPaneFormat, terminated against the executor's trim.
const programFreshFormat = "#{pane_dead}\t" + programPaneFormat + "\t" + runtimeInventoryEnd

// shadowRead takes one fresh reading of session's agent pane: one tmux
// list-panes of the session, folded to its agent pane and judged under the
// switch, with the tap liveness of the state cache's last process table. ok
// is false when the leaf is not shadowing, session is not a session name, or
// the read failed: there is nothing to compare.
func (t *Tmux) shadowRead(session string) (runtime.ProgramStatus, bool) {
	if !t.shadowing() || !validSessionNameRe.MatchString(session) {
		return runtime.ProgramStatus{}, false
	}
	out, err := t.run("list-panes", "-s", "-t", paneTarget(session), "-F", programFreshFormat)
	if err != nil {
		return runtime.ProgramStatus{}, false
	}
	var panes []programPane
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != programPaneFields+2 || fields[len(fields)-1] != runtimeInventoryEnd {
			return runtime.ProgramStatus{}, false
		}
		pane, ok := parseProgramPane(fields[1:len(fields)-1], fields[0] == "1")
		if !ok {
			return runtime.ProgramStatus{}, false
		}
		panes = append(panes, pane)
	}
	pane, named := foldProgramPane(panes)
	var taps programTaps
	if t.shadow.taps != nil {
		taps = t.shadow.taps()
	}
	return judgeProgramPane(t.programStatusMode(), t.captureGatePassed(), pane, named, taps), true
}

// shadowIdentityOf returns session's provider family and bead id, read from
// its environment at most once per shadowIdentityTTL. A failed read leaves
// both empty.
func (t *Tmux) shadowIdentityOf(session string) (family, id string) {
	sh := &t.shadow
	now := time.Now()
	sh.mu.Lock()
	cached, ok := sh.identity[session]
	sh.mu.Unlock()
	if ok && now.Sub(cached.read) < shadowIdentityTTL {
		return cached.family, cached.id
	}
	env, err := t.GetAllEnvironment(session)
	if err != nil {
		return "", ""
	}
	got := shadowIdentity{read: now, id: strings.TrimSpace(env["GC_SESSION_ID"])}
	if provider := strings.TrimSpace(env["GC_PROVIDER"]); provider != "" {
		got.family = sessionlog.ProviderFamily(provider)
	}
	sh.mu.Lock()
	if sh.identity == nil || len(sh.identity) >= 4096 {
		sh.identity = make(map[string]shadowIdentity)
	}
	sh.identity[session] = got
	sh.mu.Unlock()
	return got.family, got.id
}

// shadowEmit hands one comparison to the sink, naming the session's family
// and bead id and carrying the drops counted since the last one.
func (t *Tmux) shadowEmit(session string, point runtime.ProgramStatusPoint, scrape, program runtime.ProgramVerdict, s runtime.ProgramStatus, latency time.Duration, timed bool) {
	sink := t.shadowSink()
	if sink == nil {
		return
	}
	family, id := t.shadowIdentityOf(session)
	c := runtime.ProgramStatusComparison{
		Session: session, SessionID: id, Family: family, Point: point,
		Scrape: scrape, Program: program, Status: s,
		Dropped: int(t.shadow.dropped.Swap(0)),
	}
	if timed {
		c.Timed, c.LatencyMs = true, latency.Milliseconds()
	}
	sink(c)
}

// shadowFollow reads session's record after a decision at point until
// reached reports the program's own signal time, the reading stops being
// valid, or limit passes. It returns the last reading, the signal time, and
// whether the signal was seen. A reading that has not reached the signal is
// followed only when claimShadowFollow grants the point a follow-up;
// otherwise it is returned as is, unseen, and the decision is recorded
// untimed. It runs on a shadow goroutine, never on a decision path.
func (t *Tmux) shadowFollow(session string, point runtime.ProgramStatusPoint, s runtime.ProgramStatus, limit time.Duration, reached func(runtime.ProgramStatus) (time.Time, bool)) (runtime.ProgramStatus, time.Time, bool) {
	if at, ok := reached(s); ok {
		return s, at, true
	}
	if !s.Valid() {
		return s, time.Time{}, false
	}
	release, ok := t.claimShadowFollow(session, point)
	if !ok {
		return s, time.Time{}, false
	}
	defer release()
	start := time.Now()
	for {
		if at, ok := reached(s); ok {
			return s, at, true
		}
		if !s.Valid() {
			return s, time.Time{}, false
		}
		elapsed := time.Since(start)
		if elapsed >= limit {
			return s, time.Time{}, false
		}
		time.Sleep(shadowFollowInterval(elapsed))
		next, ok := t.shadowRead(session)
		if !ok {
			return s, time.Time{}, false
		}
		s = next
	}
}

// laterOf returns the later of a and b.
func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// shadowWaitForIdle records an idle wait's comparison (wait_for_idle): the
// scrape's verdict at decidedAt beside the record's idle verdict. A record
// not yet at a boundary is followed, up to the hold bound, to time when it
// would have let the wait return: the veto lag. A wait that ended on its
// context or a gone session decided nothing and records nothing. The settle
// of an interrupt the wait follows is recorded apart (shadowSettle).
func (t *Tmux) shadowWaitForIdle(session string, started, decidedAt time.Time, err error) {
	var scrape runtime.ProgramVerdict
	switch {
	case err == nil:
		scrape = runtime.ProgramVerdictIdle
	case errors.Is(err, ErrIdleTimeout) && errors.Is(err, runtime.ErrWaitUnobserved):
		scrape = runtime.ProgramVerdictIndeterminate
	case errors.Is(err, ErrIdleTimeout):
		scrape = runtime.ProgramVerdictBusy
	default:
		return
	}
	t.shadowGo(func() {
		s, ok := t.shadowRead(session)
		if !ok {
			return
		}
		program := runtime.ProgramIdleVerdict(s, time.Time{}, decidedAt)
		final, at, seen := s, time.Time{}, false
		if program == runtime.ProgramVerdictIdle {
			at, seen = laterOf(s.BoundaryAt(), started), true
		} else {
			final, at, seen = t.shadowFollow(session, runtime.ProgramPointWaitForIdle, s, runtime.ProgramStatusHoldLimit, func(r runtime.ProgramStatus) (time.Time, bool) {
				if !r.InputBoundary() {
					return time.Time{}, false
				}
				return laterOf(r.BoundaryAt(), started), true
			})
		}
		t.shadowEmit(session, runtime.ProgramPointWaitForIdle, scrape, program, final, at.Sub(decidedAt), seen)
	})
}

// shadowNoteInterrupt keeps the record's position as gc interrupts session,
// for the settle wait that follows (interrupt_settle). It takes one tmux read
// before the interrupt's keys.
func (t *Tmux) shadowNoteInterrupt(session string) {
	s, ok := t.shadowRead(session)
	if !ok {
		return
	}
	sh := &t.shadow
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.interrupts == nil || len(sh.interrupts) >= 4096 {
		sh.interrupts = make(map[string]shadowInterrupt)
	}
	sh.interrupts[session] = shadowInterrupt{baseline: s.Baseline(), at: time.Now()}
}

// takeShadowInterrupt returns and forgets session's interrupt baseline when
// one was noted within shadowInterruptWindow before at.
func (t *Tmux) takeShadowInterrupt(session string, at time.Time) (shadowInterrupt, bool) {
	sh := &t.shadow
	sh.mu.Lock()
	defer sh.mu.Unlock()
	mark, ok := sh.interrupts[session]
	if !ok {
		return shadowInterrupt{}, false
	}
	delete(sh.interrupts, session)
	return mark, at.Sub(mark.at) <= shadowInterruptWindow
}

// shadowSettle records an interrupt settle (interrupt_settle) when a wait
// that decided at decidedAt follows an interrupt gc noted: the wait's verdict
// (settled when it returned, unsettled at its timeout) beside the record's
// boundary past the pre-interrupt baseline, followed when it has not settled
// yet.
func (t *Tmux) shadowSettle(session string, decidedAt time.Time, err error) {
	if !t.shadowing() {
		return
	}
	mark, ok := t.takeShadowInterrupt(session, decidedAt)
	if !ok {
		return
	}
	t.shadowSettleFrom(session, mark, decidedAt, err)
}

// shadowSettleFrom is shadowSettle for an interrupt baseline already taken.
func (t *Tmux) shadowSettleFrom(session string, mark shadowInterrupt, decidedAt time.Time, err error) {
	var scrape runtime.ProgramVerdict
	switch {
	case err == nil:
		scrape = runtime.ProgramVerdictSettled
	case errors.Is(err, ErrIdleTimeout):
		scrape = runtime.ProgramVerdictUnsettled
	default:
		return
	}
	t.shadowGo(func() {
		s, ok := t.shadowRead(session)
		if !ok {
			return
		}
		program := runtime.ProgramSettleVerdict(s, mark.baseline, time.Time{}, decidedAt)
		settled := func(r runtime.ProgramStatus) (time.Time, bool) {
			if runtime.ProgramSettleVerdict(r, mark.baseline, time.Time{}, time.Now()) != runtime.ProgramVerdictSettled {
				return time.Time{}, false
			}
			return laterOf(r.BoundaryAt(), mark.at), true
		}
		final, at, seen := t.shadowFollow(session, runtime.ProgramPointInterruptSettle, s, shadowConfirmFollowLimit, settled)
		t.shadowEmit(session, runtime.ProgramPointInterruptSettle, scrape, program, final, at.Sub(decidedAt), seen)
	})
}

// shadowTypingGate records a typing gate (typing_gate): the parser look's
// verdict (lookErr, nil when it let the keys through) beside the record's
// pending verdict in s, read at the same moment.
func (t *Tmux) shadowTypingGate(session string, lookErr error, s runtime.ProgramStatus) {
	scrape := runtime.ProgramVerdictNone
	switch {
	case errors.Is(lookErr, runtime.ErrPendingInteraction):
		scrape = runtime.ProgramVerdictPending
	case lookErr != nil:
		scrape = runtime.ProgramVerdictIndeterminate
	}
	now := time.Now()
	t.shadowGo(func() {
		t.shadowEmit(session, runtime.ProgramPointTypingGate, scrape, runtime.ProgramPendingVerdict(s, time.Time{}, now), s, 0, false)
	})
}

// shadowTypingGateNow is shadowTypingGate with a reading taken now, for a
// look that refused before any baseline was needed.
func (t *Tmux) shadowTypingGateNow(session string, lookErr error) {
	if s, ok := t.shadowRead(session); ok {
		t.shadowTypingGate(session, lookErr, s)
	}
}

// shadowSubmit records a submit's confirmation (submit_confirm): the scrape's
// verdict at decidedAt (confirmed, unconfirmed, or none for a family with no
// busy indicator) beside AdvancedPast against the baseline taken before
// typing, followed when the record has not advanced yet. The program's
// signal time is when the record entered the state that credits it.
func (t *Tmux) shadowSubmit(session string, baseline runtime.ProgramBaseline, scrape runtime.ProgramVerdict, decidedAt time.Time) {
	t.shadowGo(func() {
		s, ok := t.shadowRead(session)
		if !ok {
			return
		}
		program := runtime.ProgramConfirmVerdict(s, baseline)
		advanced := func(r runtime.ProgramStatus) (time.Time, bool) {
			if !r.AdvancedPast(baseline) {
				return time.Time{}, false
			}
			if r.Running() || r.Blocked() {
				return r.Since, true
			}
			return time.Time{}, true
		}
		final, at, seen := t.shadowFollow(session, runtime.ProgramPointSubmitConfirm, s, shadowConfirmFollowLimit, advanced)
		timed := seen && !at.IsZero()
		t.shadowEmit(session, runtime.ProgramPointSubmitConfirm, scrape, program, final, at.Sub(decidedAt), timed)
	})
}

// shadowResendLook wraps an Escape-led submit's re-send look (escape_resend):
// each look's verdict (re-send while the message still waits) is recorded
// beside the record's AdvancedPast against the baseline taken before typing.
func (t *Tmux) shadowResendLook(session string, baseline runtime.ProgramBaseline, look func() (bool, error)) func() (bool, error) {
	return func() (bool, error) {
		waiting, err := look()
		if err != nil {
			return waiting, err
		}
		scrape := runtime.ProgramVerdictNoResend
		if waiting {
			scrape = runtime.ProgramVerdictResend
		}
		t.shadowGo(func() {
			s, ok := t.shadowRead(session)
			if !ok {
				return
			}
			t.shadowEmit(session, runtime.ProgramPointEscapeResend, scrape, runtime.ProgramResendVerdict(s, baseline), s, 0, false)
		})
		return waiting, err
	}
}

// shadowReadiness records a readiness wait (readiness): the scrape's verdict
// at decidedAt (ready, or not_ready at its timeout) beside the record's first
// valid report plus ProgramStatusReadySettle, followed when the program has
// not reported yet. A reading's first report is its Since while no turn has
// ended (a later state change moves Since, so then the time is not known).
func (t *Tmux) shadowReadiness(session string, decidedAt time.Time, err error) {
	scrape := runtime.ProgramVerdictReady
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return
	case err != nil:
		scrape = runtime.ProgramVerdictNotReady
	}
	t.shadowGo(func() {
		s, ok := t.shadowRead(session)
		if !ok {
			return
		}
		firstReport := func(r runtime.ProgramStatus) (time.Time, bool) {
			if !r.Valid() || r.TurnEnds > 0 || r.TurnStarts > 1 {
				return time.Time{}, false
			}
			return r.Since, !r.Since.IsZero()
		}
		first, known := firstReport(s)
		program := runtime.ProgramVerdictNotReady
		if s.Valid() {
			program = runtime.ProgramVerdictReady
			if known {
				program = runtime.ProgramReadyVerdict(s, first, decidedAt)
			}
		}
		final, at, seen := s, first, known
		if !s.Valid() {
			final, at, seen = t.shadowFollow(session, runtime.ProgramPointReadiness, s, shadowReadyFollowLimit, func(r runtime.ProgramStatus) (time.Time, bool) {
				if !r.Valid() {
					return time.Time{}, false
				}
				if since, ok := firstReport(r); ok {
					return since, true
				}
				return time.Now(), true
			})
		}
		t.shadowEmit(session, runtime.ProgramPointReadiness, scrape, program, final, at.Add(runtime.ProgramStatusReadySettle).Sub(decidedAt), seen)
	})
}

// shadowStartupBusy wraps the startup ladder's busy look (startup_ladder):
// each look's verdict (a busy pane confirms the nudge) is recorded beside the
// record's startup verdict against the baseline taken before the ladder's
// first send.
func (t *Tmux) shadowStartupBusy(session string, baseline runtime.ProgramBaseline, busy func() (bool, error)) func() (bool, error) {
	return func() (bool, error) {
		isBusy, err := busy()
		if err != nil {
			return isBusy, err
		}
		scrape := runtime.ProgramVerdictUnconfirmed
		if isBusy {
			scrape = runtime.ProgramVerdictConfirmed
		}
		now := time.Now()
		t.shadowGo(func() {
			s, ok := t.shadowRead(session)
			if !ok {
				return
			}
			t.shadowEmit(session, runtime.ProgramPointStartupLadder, scrape, runtime.ProgramStartupVerdict(s, baseline, time.Time{}, now), s, 0, false)
		})
		return isBusy, err
	}
}
