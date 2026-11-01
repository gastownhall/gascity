package runtime

import (
	"context"
	"errors"
	"time"
)

// Shadow mode (design §9): with the city switch at shadow, every decision
// point that today reads the screen also computes what the program's own
// OSC 7501 record would decide, decides with today's reading, and hands the
// pair to a [ProgramStatusComparisonSink]. The record-side rules below are
// pure functions of a reading: the cutover slices switch the same functions
// on, so the shadow measures exactly the code that will decide.

// ProgramStatusPoint names one shadow decision point (design §9.1). The
// tokens are closed: the session.program_status_shadow payload enumerates
// them.
type ProgramStatusPoint string

// The sixteen decision points.
const (
	ProgramPointWaitForIdle       ProgramStatusPoint = "wait_for_idle"
	ProgramPointTypingGate        ProgramStatusPoint = "typing_gate"
	ProgramPointSubmitConfirm     ProgramStatusPoint = "submit_confirm"
	ProgramPointEscapeResend      ProgramStatusPoint = "escape_resend"
	ProgramPointReadiness         ProgramStatusPoint = "readiness"
	ProgramPointStartupLadder     ProgramStatusPoint = "startup_ladder"
	ProgramPointInterruptSettle   ProgramStatusPoint = "interrupt_settle"
	ProgramPointPending           ProgramStatusPoint = "pending"
	ProgramPointWaitIdlePreread   ProgramStatusPoint = "wait_idle_preread"
	ProgramPointExecutionBackstop ProgramStatusPoint = "execution_backstop"
	ProgramPointClaimNudgeHold    ProgramStatusPoint = "claim_nudge_hold"
	ProgramPointWorkLadder        ProgramStatusPoint = "work_ladder"
	ProgramPointProvedIdle        ProgramStatusPoint = "proved_idle"
	ProgramPointQueuedDelivery    ProgramStatusPoint = "queued_delivery"
	ProgramPointDrainReminder     ProgramStatusPoint = "drain_reminder"
	ProgramPointObservation       ProgramStatusPoint = "observation"
)

// ProgramStatusPoints returns every decision point, in design §9.1 order.
func ProgramStatusPoints() []ProgramStatusPoint {
	return []ProgramStatusPoint{
		ProgramPointWaitForIdle, ProgramPointTypingGate, ProgramPointSubmitConfirm,
		ProgramPointEscapeResend, ProgramPointReadiness, ProgramPointStartupLadder,
		ProgramPointInterruptSettle, ProgramPointPending, ProgramPointWaitIdlePreread,
		ProgramPointExecutionBackstop, ProgramPointClaimNudgeHold, ProgramPointWorkLadder,
		ProgramPointProvedIdle, ProgramPointQueuedDelivery, ProgramPointDrainReminder,
		ProgramPointObservation,
	}
}

// RecordsOnChange reports whether p is evaluated continuously, so its
// comparisons are recorded only when the (scrape, program, validity) triple
// for a session changes, never once per evaluation (design §9.3; the
// shadow-targets P5 rule adds pending, wait_idle_preread, proved_idle and
// drain_reminder). The other points record once per decision.
func (p ProgramStatusPoint) RecordsOnChange() bool {
	switch p {
	case ProgramPointExecutionBackstop, ProgramPointClaimNudgeHold, ProgramPointWorkLadder,
		ProgramPointObservation, ProgramPointQueuedDelivery, ProgramPointPending,
		ProgramPointWaitIdlePreread, ProgramPointProvedIdle, ProgramPointDrainReminder:
		return true
	default:
		return false
	}
}

// ProgramVerdict is one side's reading at a decision point: what today's
// screen scrape decided, or what the record-side rule would decide. The
// tokens are closed: the session.program_status_shadow payload enumerates
// them.
type ProgramVerdict string

// The verdict tokens.
const (
	ProgramVerdictIdle          ProgramVerdict = "idle"
	ProgramVerdictBusy          ProgramVerdict = "busy"
	ProgramVerdictIndeterminate ProgramVerdict = "indeterminate"
	ProgramVerdictPending       ProgramVerdict = "pending"
	ProgramVerdictConfirmed     ProgramVerdict = "confirmed"
	ProgramVerdictUnconfirmed   ProgramVerdict = "unconfirmed"
	ProgramVerdictReady         ProgramVerdict = "ready"
	ProgramVerdictNotReady      ProgramVerdict = "not_ready"
	ProgramVerdictSettled       ProgramVerdict = "settled"
	ProgramVerdictUnsettled     ProgramVerdict = "unsettled"
	ProgramVerdictHold          ProgramVerdict = "hold"
	ProgramVerdictNoHold        ProgramVerdict = "no_hold"
	ProgramVerdictResend        ProgramVerdict = "resend"
	ProgramVerdictNoResend      ProgramVerdict = "no_resend"
	ProgramVerdictNone          ProgramVerdict = "none"
)

// ProgramVerdicts returns every verdict token.
func ProgramVerdicts() []ProgramVerdict {
	return []ProgramVerdict{
		ProgramVerdictIdle, ProgramVerdictBusy, ProgramVerdictIndeterminate,
		ProgramVerdictPending, ProgramVerdictConfirmed, ProgramVerdictUnconfirmed,
		ProgramVerdictReady, ProgramVerdictNotReady, ProgramVerdictSettled,
		ProgramVerdictUnsettled, ProgramVerdictHold, ProgramVerdictNoHold,
		ProgramVerdictResend, ProgramVerdictNoResend, ProgramVerdictNone,
	}
}

// ProgramStatusReadySettle is how long after its first valid report a
// prefixless program counts as ready (design §9.1, readiness).
const ProgramStatusReadySettle = 800 * time.Millisecond

// ProgramIdleVerdict is the record's idle reading (wait_for_idle,
// wait_idle_preread, observation): a live working root is busy, a live
// blocked root pending, a root at an input boundary idle. A reading that is
// not valid, or a hold past [ProgramStatus.HoldLive], is indeterminate: the
// record decides nothing, and age never reads idle. It reads the root only.
func ProgramIdleVerdict(s ProgramStatus, lastActivity, now time.Time) ProgramVerdict {
	switch {
	case s.InputBoundary():
		return ProgramVerdictIdle
	case s.Running() && s.HoldLive(lastActivity, now):
		return ProgramVerdictBusy
	case s.Blocked() && s.HoldLive(lastActivity, now):
		return ProgramVerdictPending
	default:
		return ProgramVerdictIndeterminate
	}
}

// ProgramPendingVerdict is the record's pending reading (typing_gate,
// pending): a live blocked root is pending; anything else is none. It never
// clears a parser-detected dialog: it is positive-only.
func ProgramPendingVerdict(s ProgramStatus, lastActivity, now time.Time) ProgramVerdict {
	if s.Blocked() && s.HoldLive(lastActivity, now) {
		return ProgramVerdictPending
	}
	return ProgramVerdictNone
}

// ProgramHoldVerdict is the record's veto on treating a session as quiet
// (execution_backstop, claim_nudge_hold, work_ladder, queued_delivery): a
// live working or blocked root, or a working child, holds. Past the hold
// bound, or with no valid record, nothing holds.
func ProgramHoldVerdict(s ProgramStatus, lastActivity, now time.Time) ProgramVerdict {
	busy := s.Running() || s.Blocked() || (s.Valid() && s.WorkingChildren > 0)
	if busy && s.HoldLive(lastActivity, now) {
		return ProgramVerdictHold
	}
	return ProgramVerdictNoHold
}

// ProgramBlockedHoldVerdict is the drain reminder's record hold: only a live
// blocked root holds a reminder, which typed into a dialog would answer it.
func ProgramBlockedHoldVerdict(s ProgramStatus, lastActivity, now time.Time) ProgramVerdict {
	if s.Blocked() && s.HoldLive(lastActivity, now) {
		return ProgramVerdictHold
	}
	return ProgramVerdictNoHold
}

// ProgramConfirmVerdict is the record's delivery confirmation
// (submit_confirm): confirmed only on positive evidence that the program
// advanced past the baseline taken before gc typed ([ProgramStatus.AdvancedPast]).
func ProgramConfirmVerdict(s ProgramStatus, b ProgramBaseline) ProgramVerdict {
	if s.AdvancedPast(b) {
		return ProgramVerdictConfirmed
	}
	return ProgramVerdictUnconfirmed
}

// ProgramStartupVerdict is the record's startup-ladder reading: a live
// blocked root is pending (counted as busy, so the ladder never re-types
// into a dialog), positive evidence past the pre-nudge baseline confirms,
// anything else is unconfirmed.
func ProgramStartupVerdict(s ProgramStatus, b ProgramBaseline, lastActivity, now time.Time) ProgramVerdict {
	if s.Blocked() && s.HoldLive(lastActivity, now) {
		return ProgramVerdictPending
	}
	return ProgramConfirmVerdict(s, b)
}

// ProgramResendVerdict is the record's Escape re-send reading
// (escape_resend): evidence that the program advanced past the baseline means
// the submit landed and must not be re-sent; without it the scan's rule
// stands, which re-sends.
func ProgramResendVerdict(s ProgramStatus, b ProgramBaseline) ProgramVerdict {
	if s.AdvancedPast(b) {
		return ProgramVerdictNoResend
	}
	return ProgramVerdictResend
}

// ProgramReadyVerdict is the record's readiness reading: ready once
// [ProgramStatusReadySettle] has passed since the first valid report
// (firstValid, zero when none was seen) and the reading is still valid.
func ProgramReadyVerdict(s ProgramStatus, firstValid, now time.Time) ProgramVerdict {
	if s.Valid() && !firstValid.IsZero() && now.Sub(firstValid) >= ProgramStatusReadySettle {
		return ProgramVerdictReady
	}
	return ProgramVerdictNotReady
}

// ProgramSettleVerdict is the record's interrupt-settle reading: a live
// blocked root is pending (the stop keys would answer a dialog); a root at an
// input boundary under the baseline's nonce settles when the baseline itself
// was at a boundary (nothing was running) or the record changed since it
// (Seq past the baseline's); anything else is unsettled.
func ProgramSettleVerdict(s ProgramStatus, b ProgramBaseline, lastActivity, now time.Time) ProgramVerdict {
	if s.Blocked() && s.HoldLive(lastActivity, now) {
		return ProgramVerdictPending
	}
	if s.InputBoundary() && b.Nonce != "" && s.Nonce == b.Nonce && (b.State.boundary() || s.Seq > b.Seq) {
		return ProgramVerdictSettled
	}
	return ProgramVerdictUnsettled
}

// ProgramQuiescentVerdict is the record's sleep proof (proved_idle): two
// readings, taken apart, both quiescent ([ProgramStatus.Quiescent]: a root
// boundary and no working child) under one nonce with no change between them
// prove idle. A valid pair that fails the proof is busy; a pair with an
// invalid reading proves nothing.
func ProgramQuiescentVerdict(first, second ProgramStatus) ProgramVerdict {
	if !first.Valid() || !second.Valid() {
		return ProgramVerdictIndeterminate
	}
	if first.Quiescent() && second.Quiescent() && first.Nonce == second.Nonce && first.Seq == second.Seq {
		return ProgramVerdictIdle
	}
	return ProgramVerdictBusy
}

// BoundaryAt is when the reading's root reached the input boundary it is at:
// its last turn end (QuietSince) when one is stamped, else when it entered
// its state (Since). It is zero unless the reading is at an input boundary.
func (s ProgramStatus) BoundaryAt() time.Time {
	if !s.InputBoundary() {
		return time.Time{}
	}
	if !s.QuietSince.IsZero() {
		return s.QuietSince
	}
	return s.Since
}

// ProgramStatusShadowProvider is an optional extension: a leaf reading
// program status under the shadow switch hands out the sink its comparisons
// go to, so a decision point outside the leaf records against the same
// switch and sink. It returns nil unless the switch is shadow. Composites do
// not implement it ([ProgramStatusShadowFor] resolves the leaf).
type ProgramStatusShadowProvider interface {
	ProgramStatusShadowSink() ProgramStatusComparisonSink
}

// ProgramStatusShadowFor reads session name's program status on the leaf
// serving it, for a decision point outside the leaf. ok is true only when
// that leaf is shadowing: it has a sink, and its reading was taken under the
// shadow switch. A read error, an off or on reading, or a leaf without the
// capability reports false, and the caller records nothing.
func ProgramStatusShadowFor(p Provider, name string) (ProgramStatus, ProgramStatusComparisonSink, bool) {
	leaf, _, _ := ResolveBackend(p, name)
	sh, ok := leaf.(ProgramStatusShadowProvider)
	if !ok {
		return ProgramStatus{}, nil, false
	}
	sink := sh.ProgramStatusShadowSink()
	if sink == nil {
		return ProgramStatus{}, nil, false
	}
	ps, ok := leaf.(ProgramStatusProvider)
	if !ok {
		return ProgramStatus{}, nil, false
	}
	s, err := ps.ProgramStatus(name)
	if err != nil || s.Mode != ProgramStatusShadow {
		return ProgramStatus{}, nil, false
	}
	return s, sink, true
}

// ShadowCompare records one comparison for session name at a decision
// point outside the leaf, when the leaf serving it is shadowing: scrape is
// the verdict today's code decided with, and rule maps the leaf's reading to
// the record's verdict. It reads the leaf only while shadowing (otherwise it
// does nothing at all) and hands the comparison to a sink that never blocks.
// It returns nothing: a comparison never feeds a decision.
func ShadowCompare(p Provider, name, sessionID, family string, point ProgramStatusPoint, scrape ProgramVerdict, rule func(ProgramStatus) ProgramVerdict) {
	if p == nil || name == "" {
		return
	}
	s, sink, ok := ProgramStatusShadowFor(p, name)
	if !ok {
		return
	}
	sink(ProgramStatusComparison{
		Session: name, SessionID: sessionID, Family: family, Point: point,
		Scrape: scrape, Program: rule(s), Status: s,
	})
}

// ShadowCompareHold is [ShadowCompare] for a hold rule judged with the
// runtime's last activity (claim and backstop holds, the ladder, queued
// delivery, the drain reminder). The activity is read only while the leaf is
// shadowing, and the comparison carries it with the time the rule judged at,
// so the hold liveness recorded beside the verdict is the one the rule used.
// An unreadable activity clock judges on the record's own age, which is what
// a zero activity means to [ProgramStatus.HoldLive].
//
// The rule judges at the later of now and the clock read after the reading
// and the activity: a caller passes the time its pass began, and the reading
// and activity read here can be later than that, which HoldLive would take
// for a skewed clock and hold nothing.
func ShadowCompareHold(p Provider, name, sessionID, family string, point ProgramStatusPoint, scrape ProgramVerdict, now time.Time, rule func(s ProgramStatus, lastActivity, now time.Time) ProgramVerdict) {
	if p == nil || name == "" {
		return
	}
	s, sink, ok := ProgramStatusShadowFor(p, name)
	if !ok {
		return
	}
	last, err := p.GetLastActivity(name)
	if err != nil {
		last = time.Time{}
	}
	judged := laterTime(now, time.Now())
	sink(ProgramStatusComparison{
		Session: name, SessionID: sessionID, Family: family, Point: point,
		Scrape: scrape, Program: rule(s, last, judged), Status: s,
		HoldActivity: last, JudgedAt: judged,
	})
}

// laterTime returns the later of a and b.
func laterTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// shadowCallerRecordsKey marks a context whose caller records its own
// decision point for the leaf wait it calls.
type shadowCallerRecordsKey struct{}

// WithShadowCallerRecords marks ctx: the caller records the decision it
// makes with a leaf wait at its own point (provedIdle's proved_idle), so the
// leaf records no comparison of its own for that wait. A busy seat's idle
// proof fails hundreds of times an hour; each would otherwise be a second,
// non-independent wait_for_idle sample with a follow-up of its own.
func WithShadowCallerRecords(ctx context.Context) context.Context {
	return context.WithValue(ctx, shadowCallerRecordsKey{}, true)
}

// ShadowCallerRecords reports whether ctx was marked by
// [WithShadowCallerRecords].
func ShadowCallerRecords(ctx context.Context) bool {
	marked, _ := ctx.Value(shadowCallerRecordsKey{}).(bool)
	return marked
}

// ShadowWaitIdlePreread records the wait-idle pre-read (wait_idle_preread)
// for session name: one idle snapshot (idle, busy, or indeterminate) beside
// the record's idle verdict at now. The snapshot is taken only while the
// leaf is shadowing, and a snapshot that fails otherwise records nothing.
func ShadowWaitIdlePreread(p Provider, name, sessionID, family string, now time.Time) {
	if p == nil || name == "" {
		return
	}
	s, sink, ok := ProgramStatusShadowFor(p, name)
	if !ok {
		return
	}
	snap, ok := p.(IdleSnapshotProvider)
	if !ok {
		return
	}
	scrape := ProgramVerdictBusy
	idle, err := snap.SnapshotIdle(name)
	switch {
	case errors.Is(err, ErrIdleIndeterminate):
		scrape = ProgramVerdictIndeterminate
	case err != nil:
		return
	case idle:
		scrape = ProgramVerdictIdle
	}
	sink(ProgramStatusComparison{
		Session: name, SessionID: sessionID, Family: family, Point: ProgramPointWaitIdlePreread,
		Scrape: scrape, Program: ProgramIdleVerdict(s, time.Time{}, now), Status: s,
	})
}
