package main

// The work ladder's nudges as conversation-lane notices (W5,
// PHASE1-2-EPOCH-LIVENESS-LADDER §6.6, §8).
//
// When the controller's lane host for the sessions store executes message
// ([ops] authoritative lists message) and serves the seat (servesNotice: a
// keystroke-mode runtime, or a protocol-mode one whose runtime row stamps a
// host serving message), every nudge the ladder decides is one planner
// notice from gc:planner/work-ladder (proposePlannerNotice, OJ-14's helper)
// instead of a keystroke: message{notice, boundary, live_only}, scoped to the
// row's generation, caused by the work bead, keyed
// work-ladder:<token>:<bead>[/<store>]:<episode>:<ordinal> (the entry's Since
// in Unix nanoseconds, and its Attempts+Paused after the nudge), so no key
// ever recurs. Otherwise the nudge is delivered as before (ladderDeliver).
// A route that cannot be read (the lane's op store, or the seat's runtime
// row) sends nothing: the nudge's attempt lands and counts as failed, as a
// direct nudge's delivery error does, and is reported (work.nudge_failed),
// so the seat stays paced, and escalates, recycles and releases on the
// ladder's own counts.
//
// The commit that reserves a lane nudge's attempt stamps the entry with a
// notice mark naming the op (workladder.NoticeMark). While the mark stands
// the entry is held and the seat's nudge slot taken, so the seat has at most
// one ladder notice open, and attempt n+1 waits until attempt n is settled.
// Each pass reads the marked op live and folds its fate back into the cell by
// CAS (workladder.SettleNotice): delivered counts and ends the refusals;
// failed, perhaps after reaching the seat, counts; canceled or skipped is
// withdrawn, re-counted as paused. One never delivered in its window
// (expired_before_start, open past its start_by with no attempt that may
// have reached the seat, or never proposed) is classified by its cause, as
// W3 classifies a direct nudge's error (S1): the op does not record why the
// lane held it, so the pass reads the seat live (seatRefusal). A seat at a
// pending interaction, or whose runtime is unavailable, refused it:
// re-counted as paused, with the nudge_refused alert at the third refusal in
// a row and again while they go on (workladder's refusalAlerts); one
// undelivered while the ladder's own gate holds the seat is withdrawn; any
// other cause (a content gate the ladder does not model, an incarnation
// block, a busy seat, a host that never delivered) counts as failed, so a
// wedged seat escalates, recycles and releases as on the direct path. A mark
// still unsettled ladderNoticeSettleBound after it was reserved counts as
// failed (WN4), and so does each later attempt a notice in flight that long
// keeps from being proposed (Exclusive: the seat's notices wait on it), so
// no lost op, nor one a lane never finishes, can silence a seat. The events
// follow the fold: work.nudged means the notice reached the seat.
//
// A held ladder gate (ladderGate: a stop request, a drain, a kill, a start,
// a restart, a content or operator hold, a wait, a heartbeat, a quarantine),
// proved again on a live read of the row since a cancel cannot be undone,
// withdraws the seat's pending notice, and a decision that drops a marked
// entry (its work ended, or its episode did) withdraws its op: a notice
// proposed before such a hold never lands after it (WL5), beyond one already
// in flight, which keeps the cancel as a request for the lane to decide.
// Turning the ladder off ([daemon] work_ladder = "off") withdraws every
// seat's pending notice the same way (standDownLadder), so none lands beside
// the backstops' nudges.

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/workladder"
)

const (
	// ladderNoticePurpose is the work ladder's planner-notice purpose.
	ladderNoticePurpose = "work-ladder"
	// ladderNoticePrincipal is the principal of the work ladder's notices.
	ladderNoticePrincipal = plannerNoticePrincipalPrefix + ladderNoticePurpose
)

var (
	// ladderNoticeStartWithin is a ladder notice's start_by window (2m45s):
	// OJ-14's for a reminder whose next slot is Base away.
	ladderNoticeStartWithin = plannerNoticeStartWithin(workladder.DefaultBase)
	// ladderNoticeSettleBound (4m55s) is how long after its reservation a
	// mark may stay unsettled: past it no notice is still deliverable
	// (start_by, then a full lease plus skew for one taken just before it),
	// so an op still open or unreadable counts as failed (WN4), as does each
	// attempt an op in flight that long after its proposal blocks
	// (ladderNoticeStuck).
	ladderNoticeSettleBound = ladderNoticeStartWithin + opjournal.DefaultLeaseTTL + opjournal.LeaseSkew
)

// ladderRoute is how one seat's nudges are delivered this pass: as notices
// through lane, whose op store is st; with lane nil, directly; or, when the
// route could not be read (err), not at all: the nudge is unsent and
// counts (routeUnsent).
type ladderRoute struct {
	lane plannerNoticeLane
	st   *opjournal.Store
	err  error
}

func (r *ladderRoute) viaLane() bool { return r != nil && r.lane != nil }

// unreadable reports whether the route could not be read.
func (r *ladderRoute) unreadable() bool { return r != nil && r.err != nil }

// ladderNoticeResult is what a pass read of a marked notice: still pending,
// or its fate, with the reason its event or log carries, and the generation
// the op was scoped to (zero when it could not be read). undelivered marks a
// fate of NoticeFailed for a notice that never reached the seat in its
// window, whose cause the pass has yet to classify (classifyUndelivered).
type ladderNoticeResult struct {
	op          string
	pending     bool
	fate        workladder.NoticeFate
	reason      string
	gen         int64
	undelivered bool
}

// ladderSettled is a fate a commit folded into the cell: the entry as it
// landed, the mark it cleared, the attempt and generation the mark was
// reserved at (the entry's count and the cell's generation before the fold),
// and whether it raised the refusal alert.
type ladderSettled struct {
	res     ladderNoticeResult
	entry   workladder.Entry
	mark    workladder.NoticeMark
	attempt int
	gen     int64
	alert   bool
}

// noticeLane is the lane planner notices for the pass's sessions store go to
// (plannerNoticeLaneFor), looked up once per pass.
func (p *ladderPass) noticeLane() plannerNoticeLane {
	if !p.noticesRead {
		p.noticesRead = true
		p.notices = plannerNoticeLaneFor(p.env.CityStore)
	}
	return p.notices
}

// opStore is the op store the pass reads and withdraws the ladder's notices
// through: the lane host's, or, with no host executing message (unlisted
// since the notice was proposed), one opened once per pass over the sessions
// store, read live.
func (p *ladderPass) opStore() (*opjournal.Store, error) {
	if lane := p.noticeLane(); lane != nil {
		return lane.noticeStore()
	}
	if p.ops == nil && p.opsErr == nil {
		p.ops, p.opsErr = opjournal.NewStore(beads.SessionStore{Store: laneLiveSessions{Store: p.env.CityStore}}, clock.Real{})
	}
	return p.ops, p.opsErr
}

// route is row's delivery route this pass (readRoute), or the error that kept
// it from being read: then the seat's nudge is not sent (routeUnsent).
func (p *ladderPass) route(row beads.Bead) *ladderRoute {
	r, err := p.readRoute(row)
	if err != nil {
		p.routeFailed(row, err)
		return &ladderRoute{err: err}
	}
	p.routeRecovered(row)
	return &r
}

// readRoute reads row's delivery route: the lane when a host executes message
// and serves the seat, else direct.
func (p *ladderPass) readRoute(row beads.Bead) (ladderRoute, error) {
	lane := p.noticeLane()
	if lane == nil {
		return ladderRoute{}, nil
	}
	st, err := lane.noticeStore()
	if err != nil {
		return ladderRoute{}, fmt.Errorf("opening the lane's op store: %w", err)
	}
	serves, err := lane.servesNotice(st, strings.TrimSpace(row.Metadata["session_name"]), plannerNoticeView(row))
	if err != nil {
		return ladderRoute{}, err
	}
	if !serves {
		return ladderRoute{}, nil
	}
	return ladderRoute{lane: lane, st: st}, nil
}

// routeFailed logs that row's route could not be read, at most once per
// laneErrorLogInterval: the seat's nudge is not sent.
func (p *ladderPass) routeFailed(row beads.Bead, err error) {
	if lane := p.noticeLane(); lane != nil {
		if _, logNow := lane.noticeFailed(ladderRouteTallyKey(row.ID)); !logNow {
			return
		}
	}
	fmt.Fprintf(p.l.stderr, "work-ladder: %s: reading its notice route: %v; its nudge is not sent\n", row.ID, err) //nolint:errcheck // best-effort stderr
}

// routeUnsent reports nudge n, whose attempt landed while its seat's route
// could not be read (err): it was never sent, and, as a direct nudge whose
// delivery failed, its attempt counts (S1: an unreadable route is no
// refusal by the seat).
func (p *ladderPass) routeUnsent(row beads.Bead, n workladder.Nudge, gen int64, err error) {
	fields := workLadderEventFields{Kind: n.Kind.String(), Rung: n.Rung.String(), Attempt: n.Attempt, Paused: n.Paused, Reason: "notice route unreadable: " + err.Error()}
	p.event(events.WorkNudgeFailed, row, n.Key, gen, fields)
}

// routeRecovered clears row's route-failure tally.
func (p *ladderPass) routeRecovered(row beads.Bead) {
	if lane := p.noticeLane(); lane != nil {
		lane.noticeRecovered(ladderRouteTallyKey(row.ID))
	}
}

func ladderRouteTallyKey(conv string) string {
	return strings.Join([]string{conv, ladderNoticePurpose, "route"}, "\x00")
}

// ladderProposeTallyKey is the key of conv's proposal-failure tally: one per
// seat, since each attempt's notice has its own key and a per-notice tally
// would grow by one entry per refused attempt through an outage.
func ladderProposeTallyKey(conv string) string {
	return strings.Join([]string{conv, ladderNoticePurpose, "propose"}, "\x00")
}

// ladderNotice is the notice of e's nudge about key, carrying text, for
// session row row: its key's episode is e's Since, its ordinal e's nudges
// this episode (Attempts+Paused, counted or not), so each reservation has its
// own key.
func ladderNotice(key workladder.ClaimKey, e workladder.Entry, text string) plannerNotice {
	episode := "0"
	if !e.Since.IsZero() {
		episode = strconv.FormatInt(e.Since.UnixNano(), 10)
	}
	return plannerNotice{
		Purpose:     ladderNoticePurpose,
		Trigger:     backstopNoticeTrigger(backstopTarget{ID: key.Bead, StoreRef: key.Store}),
		Causation:   key.Bead,
		Episode:     episode,
		Attempt:     max(1, e.Attempts+e.Paused),
		Content:     text,
		StartWithin: ladderNoticeStartWithin,
		Exclusive:   true,
	}
}

// ladderNoticeOpID is the op id proposePlannerNotice derives for e's nudge
// about key on session row row, through op store st.
func ladderNoticeOpID(st *opjournal.Store, row beads.Bead, key workladder.ClaimKey, e workladder.Entry) (string, error) {
	return opjournal.DeriveID(st.Prefix(), row.ID, ladderNoticePrincipal, plannerNoticeKey(ladderNotice(key, e, ""), plannerNoticeToken(row)))
}

// ladderNoticeFate reads mark m's op (op, or the read's err) at now. A
// notice that never reached the seat in its window is settled failed and
// undelivered, for its caller to classify by cause; one the lane closed
// provider_unavailable is refused: the lane recorded the runtime
// unavailable.
func ladderNoticeFate(op opjournal.Op, err error, m workladder.NoticeMark, now time.Time) ladderNoticeResult {
	r := ladderNoticeResult{op: m.Op}
	if err == nil {
		r.gen = op.Scope.Generation
	}
	settled := func(f workladder.NoticeFate, reason string) ladderNoticeResult {
		r.fate, r.reason = f, reason
		return r
	}
	undelivered := func(why string) ladderNoticeResult {
		r.undelivered = true
		return settled(workladder.NoticeFailed, why)
	}
	unsettled := func(why string) ladderNoticeResult {
		if now.Before(m.At.Add(ladderNoticeSettleBound)) {
			r.pending = true
			return r
		}
		return settled(workladder.NoticeFailed, "notice unsettled: "+why)
	}
	switch {
	case errors.Is(err, beads.ErrNotFound):
		return undelivered("notice not proposed")
	case err != nil:
		return unsettled(err.Error())
	case op.Status == opjournal.StatusClosed:
		arm, reason := op.Outcome.Effective()
		switch arm {
		case opjournal.ArmSucceeded:
			return settled(workladder.NoticeDelivered, "")
		case opjournal.ArmCanceled:
			why := "canceled by " + op.Cancel.By
			if op.Cancel.Reason != "" {
				why += ": " + op.Cancel.Reason
			}
			return settled(workladder.NoticeWithdrawn, why)
		case opjournal.ArmSkipped:
			return settled(workladder.NoticeWithdrawn, "skipped{"+reason+"}")
		}
		why := "notice failed{" + reason + "}"
		if op.LastError != "" {
			why += ": " + op.LastError
		}
		switch reason {
		case opjournal.ReasonProviderUnavailable:
			return settled(workladder.NoticeRefused, why)
		case opjournal.ReasonExpiredBeforeStart:
			return undelivered(why)
		}
		return settled(workladder.NoticeFailed, why)
	case plannerNoticeInFlight(op):
		return unsettled("in flight")
	case !op.StartBy.IsZero() && !now.Before(op.StartBy):
		if op.MaybeDelivered > 0 {
			return settled(workladder.NoticeFailed, "notice past its start_by after an attempt that may have reached the seat")
		}
		return undelivered("notice past its start_by undelivered")
	}
	return unsettled("waiting for a boundary")
}

// withdrawGate is the gate the held-gate withdraw (noticeOutcomes) acts on:
// gate, the snapshot's, proved again on a live read of row when it holds a
// seat whose cell prev carries a mark, since a withdraw cannot be undone. A
// hold the row no longer carries withdraws nothing. The error is a read that
// did not answer.
func (p *ladderPass) withdrawGate(row beads.Bead, prev workladder.Record, gate workladder.Gate) (workladder.Gate, error) {
	if !gate.Held || !slices.ContainsFunc(prev.Entries, func(e workladder.Entry) bool { return !e.Notice.IsZero() }) {
		return gate, nil
	}
	fresh, err := beads.HandlesFor(p.env.CityStore).Live.Get(row.ID)
	if err != nil {
		return workladder.Gate{}, err
	}
	return ladderGate(sessionInfoFromBead(fresh), fresh.Metadata, p.now), nil
}

// noticeOutcomes reads the fate of every mark prev carries, live. While gate
// holds the seat it withdraws each pending notice first (WL5): one not yet
// started, or never proposed, is withdrawn; one in flight keeps the cancel
// as a request and stays pending. A notice that never reached the seat is
// classified by its cause (classifyUndelivered). It returns the settled
// marks.
func (p *ladderPass) noticeOutcomes(row beads.Bead, prev workladder.Record, gate workladder.Gate) []ladderNoticeResult {
	var marks []workladder.NoticeMark
	for _, e := range prev.Entries {
		if !e.Notice.IsZero() {
			marks = append(marks, e.Notice)
		}
	}
	if len(marks) == 0 {
		return nil
	}
	st, stErr := p.opStore()
	var out []ladderNoticeResult
	refusal := sync.OnceValue(func() string { return p.seatRefusal(row) })
	for _, m := range marks {
		op, err := opjournal.Op{}, stErr
		if stErr == nil {
			op, err = st.Get(m.Op)
		}
		res := ladderNoticeFate(op, err, m, p.now)
		if gate.Held && stErr == nil && (res.pending || errors.Is(err, beads.ErrNotFound)) {
			res = p.withdrawHeld(row, st, m, gate.Reason, res)
		}
		if res.undelivered {
			res = classifyUndelivered(res, gate, refusal)
		}
		if !res.pending {
			out = append(out, res)
		}
	}
	return out
}

// classifyUndelivered is res, a notice that never reached the seat in its
// window, settled by its cause (S1, W3 parity): withdrawn while gate, the
// ladder's own, holds the seat; refused when the seat refuses nudges now
// (refusal: a pending interaction, the runtime unavailable); else failed, as
// res stands, so a cause the ladder does not see still counts.
func classifyUndelivered(res ladderNoticeResult, gate workladder.Gate, refusal func() string) ladderNoticeResult {
	res.undelivered = false
	switch {
	case gate.Held:
		res.fate, res.reason = workladder.NoticeWithdrawn, res.reason+"; the work ladder holds the seat: "+gate.Reason
	default:
		if cause := refusal(); cause != "" {
			res.fate, res.reason = workladder.NoticeRefused, res.reason+"; the seat refuses nudges: "+cause
		}
	}
	return res
}

// seatRefusal is why row's seat cannot take a nudge now, read live as W3's
// direct delivery finds it (ladderPass.nudge's refusals): its runtime
// unavailable, or a pending interaction only a human can answer. It is ""
// for neither, and for a read that answered with any other error.
func (p *ladderPass) seatRefusal(row beads.Bead) string {
	name := strings.TrimSpace(row.Metadata["session_name"])
	sp := p.env.SP
	if sp == nil || name == "" {
		return runtime.ErrRuntimeUnavailable.Error()
	}
	ip, ok := sp.(runtime.InteractionProvider)
	if !ok {
		return ""
	}
	pending, err := ip.Pending(name)
	switch {
	case errors.Is(err, runtime.ErrRuntimeUnavailable), errors.Is(err, runtime.ErrPendingInteraction):
		return err.Error()
	case err != nil || pending == nil:
		return ""
	}
	return runtime.ErrPendingInteraction.Error()
}

// withdrawHeld cancels mark m's op because the ladder's gate holds the seat
// for reason; res is what the read found.
func (p *ladderPass) withdrawHeld(row beads.Bead, st *opjournal.Store, m workladder.NoticeMark, reason string, res ladderNoticeResult) ladderNoticeResult {
	why := "the work ladder holds the seat: " + reason
	result, _, err := st.Cancel(m.Op, ladderNoticePrincipal, why)
	switch {
	case errors.Is(err, beads.ErrNotFound):
		return ladderNoticeResult{op: m.Op, fate: workladder.NoticeWithdrawn, reason: "notice not proposed; " + why}
	case err != nil:
		fmt.Fprintf(p.l.stderr, "work-ladder: %s: withdrawing notice %s: %v\n", row.ID, m.Op, err) //nolint:errcheck // best-effort stderr
		return res
	case result == opjournal.CancelClosed:
		return ladderNoticeResult{op: m.Op, fate: workladder.NoticeWithdrawn, reason: "canceled by " + ladderNoticePrincipal + ": " + why}
	}
	return res
}

// foldLadderNotices is r with each settled fate folded in, and what landed.
func foldLadderNotices(r workladder.Record, outs []ladderNoticeResult) (workladder.Record, []ladderSettled) {
	var settled []ladderSettled
	for _, o := range outs {
		i := slices.IndexFunc(r.Entries, func(e workladder.Entry) bool { return e.Notice.Op == o.op })
		if i < 0 {
			continue
		}
		m, attempt := r.Entries[i].Notice, r.Entries[i].Attempts
		next, alert, ok := workladder.SettleNotice(r, o.op, o.fate)
		if !ok {
			continue
		}
		r = next
		settled = append(settled, ladderSettled{res: o, entry: r.Entries[i], mark: m, attempt: attempt, gen: r.Gen, alert: alert})
	}
	return r, settled
}

// droppedMarks are the ops of prev's marks that next no longer carries: the
// decision dropped their entry, or ended its episode.
func droppedMarks(prev, next workladder.Record) []string {
	var out []string
	for _, e := range prev.Entries {
		if e.Notice.IsZero() {
			continue
		}
		if !slices.ContainsFunc(next.Entries, func(n workladder.Entry) bool { return n.Notice.Op == e.Notice.Op }) {
			out = append(out, e.Notice.Op)
		}
	}
	return out
}

// noticeSettled reports a fate the commit folded in: work.nudged for a
// notice that reached the seat, work.nudge_failed (and the refusal alert at
// the third in a row, then periodically) for one that failed or was refused,
// a log line for one withdrawn. As a direct nudge's, the events carry the
// attempt the notice was reserved as, not one a refusal re-counted, and the
// generation it was scoped to (the cell's before the fold when the op could
// not be read), not the one the fold landed at.
func (p *ladderPass) noticeSettled(row beads.Bead, s ladderSettled) {
	gen := s.gen
	if s.res.gen > 0 {
		gen = s.res.gen
	}
	e := s.entry
	fields := workLadderEventFields{Kind: e.Kind.String(), Rung: e.Rung.String(), Attempt: s.attempt, Paused: s.mark.Paused}
	switch s.res.fate {
	case workladder.NoticeDelivered:
		p.event(events.WorkNudged, row, e.Key, gen, fields)
	case workladder.NoticeWithdrawn:
		fmt.Fprintf(p.l.stderr, "work-ladder: %s: notice %s about %s withdrawn: %s\n", row.ID, s.res.op, e.Key.Bead, s.res.reason) //nolint:errcheck // best-effort stderr
	default:
		fields.Reason = s.res.reason
		fmt.Fprintf(p.l.stderr, "work-ladder: %s: notice %s about %s: %s\n", row.ID, s.res.op, e.Key.Bead, s.res.reason) //nolint:errcheck // best-effort stderr
		p.event(events.WorkNudgeFailed, row, e.Key, gen, fields)
		if s.alert {
			p.event(events.WorkEscalated, row, e.Key, gen, workLadderEventFields{Reason: workladder.AlertNudgeRefused})
		}
	}
}

// withdrawDropped cancels the ops of marks a decision dropped, best-effort:
// an op never proposed is nothing to withdraw.
func (p *ladderPass) withdrawDropped(row beads.Bead, ops []string) {
	if len(ops) == 0 {
		return
	}
	st, err := p.opStore()
	if err != nil {
		fmt.Fprintf(p.l.stderr, "work-ladder: %s: withdrawing its notices: %v\n", row.ID, err) //nolint:errcheck // best-effort stderr
		return
	}
	for _, id := range ops {
		if _, _, err := st.Cancel(id, ladderNoticePrincipal, "the work ladder dropped its reminder"); err != nil && !errors.Is(err, beads.ErrNotFound) {
			fmt.Fprintf(p.l.stderr, "work-ladder: %s: withdrawing notice %s: %v\n", row.ID, id, err) //nolint:errcheck // best-effort stderr
		}
	}
}

// proposeNotice proposes n's notice, whose attempt landed marked m, through
// route, and settles the mark at once when the proposal cannot stand: the
// route changed under the pass or a ladder notice is in flight (withdrawn),
// or the op store refuses the request itself, or the notice in flight has
// stood past its bound (failed: it counts, as a delivery error does). Any
// other error leaves the mark: the op may have landed though the call failed
// (a create whose follow-up read failed), so the next pass's fold reads it
// by id and settles its real fate, or, if it never landed, settles it never
// proposed.
func (p *ladderPass) proposeNotice(row beads.Bead, n workladder.Nudge, e workladder.Entry, gen int64, m workladder.NoticeMark, route *ladderRoute) {
	notice := ladderNotice(n.Key, e, p.nudgeText(row, n))
	out, op, err := proposePlannerNotice(route.lane, row, notice)
	fields := workLadderEventFields{Kind: n.Kind.String(), Rung: n.Rung.String(), Attempt: n.Attempt, Paused: n.Paused}
	tally := ladderProposeTallyKey(row.ID)
	switch {
	case err != nil && plannerNoticeRefused(err):
		fields.Reason = "notice refused: " + err.Error()
		fmt.Fprintf(p.l.stderr, "work-ladder: %s: proposing its notice about %s: %v\n", row.ID, n.Key.Bead, err) //nolint:errcheck // best-effort stderr
		p.event(events.WorkNudgeFailed, row, n.Key, gen, fields)
		p.settleMark(row.ID, m.Op, workladder.NoticeFailed)
	case err != nil:
		if _, logNow := route.lane.noticeFailed(tally); logNow {
			fmt.Fprintf(p.l.stderr, "work-ladder: %s: proposing its notice about %s: %v; the next pass settles notice %s\n", row.ID, n.Key.Bead, err, m.Op) //nolint:errcheck // best-effort stderr
		}
	case out == plannerNoticePending && ladderNoticeStuck(op, p.now):
		// WN4: the seat's notices wait on this one (Exclusive) for as long as
		// it stays in flight, so each attempt it blocks past its bound counts,
		// as a mark unsettled that long does: a lane that never finishes it
		// cannot keep the seat from recycling, releasing and escalating.
		fields.Reason = "notice not proposed: ladder notice " + op.ID + " in flight past its bound"
		fmt.Fprintf(p.l.stderr, "work-ladder: %s: proposing its notice about %s: %s\n", row.ID, n.Key.Bead, fields.Reason) //nolint:errcheck // best-effort stderr
		p.event(events.WorkNudgeFailed, row, n.Key, gen, fields)
		p.settleMark(row.ID, m.Op, workladder.NoticeFailed)
	case out == plannerNoticeDirect, out == plannerNoticePending:
		why := "no lane serves the seat now"
		if out == plannerNoticePending {
			why = "ladder notice " + op.ID + " is in flight"
		}
		fmt.Fprintf(p.l.stderr, "work-ladder: %s: notice about %s withdrawn: %s\n", row.ID, n.Key.Bead, why) //nolint:errcheck // best-effort stderr
		p.settleMark(row.ID, m.Op, workladder.NoticeWithdrawn)
	case op.ID != m.Op:
		fmt.Fprintf(p.l.stderr, "work-ladder: %s: notice %s about %s is not its mark %s; withdrawing it\n", row.ID, op.ID, n.Key.Bead, m.Op) //nolint:errcheck // best-effort stderr
		if _, _, err := route.st.Cancel(op.ID, ladderNoticePrincipal, "not the work ladder's mark "+m.Op); err != nil {
			fmt.Fprintf(p.l.stderr, "work-ladder: %s: withdrawing notice %s: %v\n", row.ID, op.ID, err) //nolint:errcheck // best-effort stderr
		}
		p.settleMark(row.ID, m.Op, workladder.NoticeWithdrawn)
	default:
		route.lane.noticeRecovered(tally)
		fmt.Fprintf(p.l.stderr, "work-ladder: %s: queued notice %s about %s\n", row.ID, op.ID, n.Key.Bead) //nolint:errcheck // best-effort stderr
	}
}

// ladderNoticeStuck reports whether op, a ladder notice a lane has started,
// has stood ladderNoticeSettleBound since it was proposed: as for a mark
// unsettled that long (WN4), no lane still delivers it in its window; its
// host is wedged, or renews a lease it never finishes.
func ladderNoticeStuck(op opjournal.Op, now time.Time) bool {
	return !now.Before(op.CreatedAt.Add(ladderNoticeSettleBound))
}

// settleMark settles row id's mark op with fate, one that raises no alert
// (failed or withdrawn), by CAS.
func (p *ladderPass) settleMark(id, op string, fate workladder.NoticeFate) {
	p.settle(id, func(r workladder.Record) (workladder.Record, bool) {
		out, _, ok := workladder.SettleNotice(r, op, fate)
		return out, ok
	})
}
