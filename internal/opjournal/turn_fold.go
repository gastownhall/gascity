package opjournal

import (
	"slices"
	"strings"
	"time"
)

// TurnState is the state of the turn cells (gc.rt.turn.state). This build
// writes only the values below. The empty value is a row whose observer has
// not yet written the cells, and a newer build's value decodes as read; readers
// treat both as TurnStateUnknown.
type TurnState string

// Turn states.
const (
	// TurnStateRunning: the turn named by gc.rt.turn.id was running when the
	// observer last looked.
	TurnStateRunning TurnState = "running"
	// TurnStateEnded: no turn was running, and gc.rt.turn.last_* name the
	// last one seen to end.
	TurnStateEnded TurnState = "ended"
	// TurnStateUnknown: the observer has nothing to say about the current
	// generation yet.
	TurnStateUnknown TurnState = "unknown"
)

var turnStates = []TurnState{TurnStateRunning, TurnStateEnded, TurnStateUnknown}

// TurnEndReason is the reason of a turn.ended fact and of the
// gc.rt.turn.last_end cell (X11 D8). Reasons are OJ-1 tokens, open and
// additive: a reader keeps a reason it does not know.
type TurnEndReason string

// Turn end reasons.
const (
	// TurnEndCompleted: the agent finished.
	TurnEndCompleted TurnEndReason = "completed"
	// TurnEndInterrupted: a gc interrupt op ended it. The fact's CauseOps
	// names the op.
	TurnEndInterrupted TurnEndReason = "interrupted"
	// TurnEndInterruptedLocally: an interrupt gc did not send, such as an
	// attached human, or a runtime interrupting while gc was down.
	TurnEndInterruptedLocally TurnEndReason = "interrupted_locally"
	// TurnEndFailed: a provider or API error or an undeliverable prompt.
	TurnEndFailed TurnEndReason = "failed"
	// TurnEndSuperseded: a newer turn started before any end was observed,
	// and the source shows no interrupt marker.
	TurnEndSuperseded TurnEndReason = "superseded"
	// TurnEndEnded: over, but how is unknown; an inferred end with no gc
	// interrupt involved.
	TurnEndEnded TurnEndReason = "ended"
	// TurnEndSessionGone: the runtime exited mid-turn.
	TurnEndSessionGone TurnEndReason = "session_gone"
	// TurnEndRuntimeRestart: the runtime restarted mid-turn.
	TurnEndRuntimeRestart TurnEndReason = "runtime_restart"
	// TurnEndUnobserved: the observer lost track; the turn left the source's
	// recent window before it was read.
	TurnEndUnobserved TurnEndReason = "unobserved"
)

var turnEndReasons = []TurnEndReason{
	TurnEndCompleted, TurnEndInterrupted, TurnEndInterruptedLocally, TurnEndFailed, TurnEndSuperseded,
	TurnEndEnded, TurnEndSessionGone, TurnEndRuntimeRestart, TurnEndUnobserved,
}

// TurnEndReasons returns the turn end reasons this build writes.
func TurnEndReasons() []TurnEndReason { return slices.Clone(turnEndReasons) }

// TurnSourceStatus is how a turn's source of record says it ended. The first
// three mirror the runtime's turn statuses; the observer converts. The empty
// value is a turn the source closed only because a newer one started.
type TurnSourceStatus string

// Turn source statuses.
const (
	TurnSourceCompleted TurnSourceStatus = "completed"
	TurnSourceCancelled TurnSourceStatus = "cancelled" //nolint:misspell // matches the runtime's wire spelling
	TurnSourceFailed    TurnSourceStatus = "failed"
	// TurnSourceRestarted is a source that reports the runtime itself
	// restarted mid-turn.
	TurnSourceRestarted TurnSourceStatus = "restarted"
)

// TurnCapsPair is one key=value pair of the turn capabilities stamp.
type TurnCapsPair struct {
	Key   string
	Value string
}

// TurnCapsStamp is the stored form of the turn capabilities stamp
// (gc.rt.turn.caps): the turn axes, then source, interrupt and replace, in the
// observer's order, as k=v pairs joined by ",". Keys and values are tokens.
// The observer writes it at the first observation of a generation and at
// every downgrade; the codec does not interpret it. New keys and values are
// additive and decode on every build; a change to the k=v syntax is a schema
// change (gc.rt.schema), not a new value. The typed capability axes
// the lane reads (DES-O1 §4.2's Caps.Turns) are parsed from it at that edge
// and are not this type.
type TurnCapsStamp []TurnCapsPair

// TurnCells are the runtime row's gc.rt.turn.* cells. The turn observer is
// their only writer (D-TURN). They are observations: hints with an age, never
// proof that a turn is or is not running. Every time cell is the observer's
// clock (X11 §8, D9; DES-O1 §5.1): At and LastAt are when the observer saw
// the transition, not when the source says it happened, and ObservedAt is
// the observation that last changed the cells. The source's own times live
// on the turn facts.
type TurnCells struct {
	// Generation is the session the cells describe (gc.rt.turn.gen). A
	// reader whose current generation is another reads every cell as
	// unknown.
	Generation int64
	// Caps is the capabilities stamp (gc.rt.turn.caps).
	Caps TurnCapsStamp
	// ID is the running turn's source-owned id while State is running, and
	// empty otherwise (gc.rt.turn.id). EncodeRuntimeRow stores any text, but
	// RuntimeRowPatch refuses an id that spells a JSON number, boolean or
	// null, since a fenced update would not store it as a string; the same
	// holds for LastID and Seen. Sources whose ids can look like that must
	// make them non-scalar before folding.
	ID string
	// State is gc.rt.turn.state.
	State TurnState
	// At is when the observer saw the current state begin: the running
	// turn's start, or the last end (gc.rt.turn.at).
	At time.Time
	// LastID, LastEnd and LastAt name the last turn seen to end, its reason
	// and when the observer saw it end (gc.rt.turn.last_id, last_end,
	// last_at).
	LastID  string
	LastEnd TurnEndReason
	LastAt  time.Time
	// Seen is the fold's anchor in the source's window: the newest ended
	// turn of the last Recent it folded (gc.rt.turn.seen). Turns of Recent
	// newer than Seen are new; when Seen has left the window, all of Recent
	// is. That holds only while the caller keeps an ended turn, an inferred
	// end included, in Recent until it ages out or is current again
	// (TurnObservation.Recent); an end the fold itself infers at idle is the
	// exception (TurnObservation.InferIdleEnds). It is fold bookkeeping, not
	// an observation for readers: LastID can name a turn Recent never holds
	// (one ended unobserved, session_gone or inferred at idle), so it cannot
	// anchor the window.
	Seen string
	// ObservedAt is the observer's clock at the observation that last
	// changed the cells (gc.rt.turn.observed_at).
	ObservedAt time.Time
}

func (c TurnCells) equal(o TurnCells) bool {
	return c.Generation == o.Generation && slices.Equal(c.Caps, o.Caps) && c.ID == o.ID &&
		c.State == o.State && c.At.Equal(o.At) && c.LastID == o.LastID && c.LastEnd == o.LastEnd &&
		c.LastAt.Equal(o.LastAt) && c.Seen == o.Seen && c.ObservedAt.Equal(o.ObservedAt)
}

// running reports the turn the cells say is running, or "".
func (c TurnCells) running() string {
	if c.State == TurnStateRunning {
		return c.ID
	}
	return ""
}

// ObservedTurnEnd is one turn the source saw end.
type ObservedTurnEnd struct {
	// ID is the source-owned turn id.
	ID string
	// StartedAt and EndedAt are the source's times; zero when unknown.
	// EndedAt is the observer's clock for an inferred end.
	StartedAt time.Time
	EndedAt   time.Time
	Status    TurnSourceStatus
	// Inferred is an end read off an idle boundary rather than recorded.
	Inferred bool
	// StopReason is the provider's own stop reason, if any. It becomes the
	// fact's Detail, lower-cased; one that is still not a token is left
	// out, since Detail is optional and nothing decides on it.
	StopReason string
}

// TurnObservation is one read of a session's turns, converted by the
// observer from its source.
type TurnObservation struct {
	Conversation string
	// Generation is the session the read was made in.
	Generation int64
	// Current is the running turn's id, or "" when idle.
	Current string
	// CurrentStart is the running turn's source start; zero when unknown.
	CurrentStart time.Time
	// Recent is the source's ended turns, newest first, at most the
	// source's bound. It holds only turns of Generation: the caller filters
	// out an earlier incarnation's turns (a resumed transcript keeps them),
	// because a new generation's fold journals every turn of Recent under
	// Generation, and the generation is part of each fact's id.
	//
	// An ended turn, an inferred end included, stays in Recent until it
	// ages out of the source's bound or its turn is current again. The
	// fold anchors on the newest ended turn it last read (TurnCells.Seen)
	// and reads an anchor missing from Recent as aged out, so every turn
	// of Recent is new. A source whose inferred end is recomputed on each
	// read (X11 §5.2's idle overlay) can drop it otherwise, for example when
	// the session dies right after the inference; the caller carries
	// such an end forward, or the fold journals the window's turns again.
	// An end the fold itself infers at idle (InferIdleEnds) is the
	// exception: the source kept no record of it, so Recent does not hold
	// it.
	Recent []ObservedTurnEnd
	// SessionGone reports that the runtime exited.
	SessionGone bool
	// InferIdleEnds reports a source that keeps no record of some ends, so
	// they are inferred at the idle boundary: a turn reads as running and
	// then, idle, as in neither Current nor Recent. For such a source a
	// running turn the observation shows idle with that turn missing, and
	// no ended turn newer than Seen but the one journaled last (LastID),
	// ended normally: the fold journals an inferred end (InferredIdleEnd),
	// not an unobserved one. A newer turn, current or in Recent, while the
	// running turn's end was never seen is still a gap. The caller sets it
	// only when its own observation of the source saw that turn running; a
	// reader that did not (a restarted or successor observer) cannot tell a
	// normal end from a gap, so it leaves it unset and the end is
	// unobserved. An end inferred so is the exception to Recent's contract:
	// Recent does not hold it, so Seen never anchors on it, and a source
	// that restores the turn only after the next one ran shows it newer
	// than Seen. While that turn is LastID the fold reads it as no later
	// turn; once a later turn's end is journaled too, it reads as new
	// (FoldTurn, on ends recorded late), and the running turn's idle end as
	// a gap.
	InferIdleEnds bool
	// ObservedAt is the observer's clock at the read. It is required and
	// non-zero: it dates every cell write and every fact without a source
	// time. FoldTurn does not check it; a zero time leaves the time cells
	// empty, and a fact it dates does not encode.
	ObservedAt time.Time
	// Caps is the capabilities stamp to publish; nil keeps the stored one.
	Caps TurnCapsStamp
}

// InterruptRef names the interrupt gc is running on a turn, so the fold
// credits that turn's end to the op. A ref without both an op and a turn is
// no interrupt: an interrupted end always names its op (X11 D5).
type InterruptRef struct {
	// Op is the interrupt op's journal id. The fold copies it into the
	// turn.ended fact's CauseOps unchecked, and a fact whose cause is not an
	// op id does not encode.
	Op string
	// Turn is the source-owned id of the interrupted turn.
	Turn string
}

// names reports whether r is an interrupt op on turn.
func (r InterruptRef) names(turn string) bool { return r.Op != "" && r.Turn != "" && r.Turn == turn }

// FoldTurn folds one observation into the turn cells and returns the facts to
// create (if absent) in the same batch. Unchanged state yields no write, and
// prev comes back as is. It is pure: TI-3's observer converts its source's
// history into obs, calls it, and is the only caller (D-TURN).
//
// The rules (X11 §5.3):
//   - An observation of an older generation is stale and writes nothing. A
//     newer one resets the cells; the old generation's open turn is left to
//     session.ended.
//   - The running turn T ends when the observation no longer shows it
//     current: with its recorded reason when T is in Recent; session_gone
//     when the session is gone; as an inferred end when the source infers
//     idle ends and the observation is idle with no turn newer than Seen
//     but LastID, which then moves no cell back (InferredIdleEnd); else
//     unobserved.
//   - Turns in Recent newer than the cells' Seen anchor and never seen
//     running get both facts, oldest first. When Seen has left the window
//     all of Recent is new; when Seen is the current turn again none is.
//   - A current turn that is not the running one starts.
//   - The last ended turn (LastID) has its facts already. When it is current
//     again (X11 §8's false idle) the cells go back to running with no
//     fact, and when it ends again the cells follow the source with no
//     second turn.ended; the first one stays as the record of what gc
//     concluded.
//
// LastID is all the fold remembers of what it journaled. A source that
// records ends late can show a turn the fold already ended unobserved as
// newer than Seen once a later turn's end is journaled too; the fold then
// returns that turn's facts again, with the recorded reason. When the later
// turn is not in Recent as ended (its end is not yet recorded, or it is
// running again after a false idle), LastID moves back to the older turn,
// and the later turn's facts can come again as well. The caller writes each
// fact create-if-absent, as RecordFact does: an occurrence already stored is
// success and keeps its first record.
//
// Folding the same observation twice writes nothing the second time.
//
// Ended reasons: completed is completed; a canceled turn is interrupted when
// interrupting names the turn (CauseOps then names the op) and
// interrupted_locally otherwise; failed is failed, or session_gone when the
// session also ended; restarted is runtime_restart; no status is superseded;
// an inferred end is interrupted when interrupting names the turn and ended
// otherwise, with basis observed. A status this build does not know reads as
// ended.
func FoldTurn(prev TurnCells, obs TurnObservation, interrupting InterruptRef) (next TurnCells, facts []Fact, write bool) {
	if obs.Generation < 1 || obs.Generation < prev.Generation {
		return prev, nil, false
	}
	next = prev
	if obs.Generation != prev.Generation {
		// Capabilities are per generation too: the new one starts unstamped.
		next = TurnCells{Generation: obs.Generation, State: TurnStateUnknown}
	}
	if obs.Caps != nil {
		next.Caps = slices.Clone(obs.Caps)
	}
	f := folder{obs: obs, interrupting: interrupting, next: &next, journaled: next.LastID}

	runningTurn := next.running()
	unseen := obs.endedAfter(next.Seen)
	if runningTurn != "" && obs.Current != runningTurn {
		if i := slices.IndexFunc(unseen, func(e ObservedTurnEnd) bool { return e.ID == runningTurn }); i >= 0 {
			// Recorded: its end comes in order with the other unseen turns.
			f.endUnseen(unseen[:i], runningTurn)
			f.end(unseen[i])
			unseen = unseen[i+1:]
		} else if e, ok := obs.recent(runningTurn); ok {
			f.end(e)
		} else if obs.InferredIdleEnd(next) == runningTurn {
			f.end(ObservedTurnEnd{ID: runningTurn, EndedAt: obs.ObservedAt, Inferred: true})
			// unseen can hold only the journaled turn, restored after this
			// one ran (InferredIdleEnd): it ended first and moves no cell back.
			unseen = nil
		} else {
			f.endMissing(runningTurn)
		}
		runningTurn = ""
	}
	f.endUnseen(unseen, runningTurn)

	switch {
	case obs.Current != "" && obs.Current != runningTurn:
		if obs.Current != f.journaled {
			at := orTime(obs.CurrentStart, obs.ObservedAt)
			f.facts = append(f.facts, f.fact(FactTurnStarted, obs.Current, at, FactBasisRuntime))
		}
		next.ID, next.State, next.At = obs.Current, TurnStateRunning, obs.ObservedAt
	case obs.Current == "" && f.ended:
		next.ID, next.State, next.At = "", TurnStateEnded, next.LastAt
	}
	if len(obs.Recent) > 0 {
		next.Seen = obs.Recent[0].ID
	}

	if len(f.facts) == 0 && next.equal(prev) {
		return prev, nil, false
	}
	next.ObservedAt = obs.ObservedAt
	return next, f.facts, true
}

// InferredIdleEnd is the turn the cells prev show running whose end the
// fold of o infers at the idle boundary (InferIdleEnds), or "": o is of
// prev's generation from a source that infers idle ends, idle with the
// session not gone, and shows neither that turn nor any ended turn newer
// than prev's Seen anchor but prev's LastID. That one is journaled already:
// an inferred end never moves Seen, so a source that restores the turn
// only after the next one ran shows it newer than Seen, yet it ended first.
func (o TurnObservation) InferredIdleEnd(prev TurnCells) string {
	turn := prev.running()
	if !o.InferIdleEnds || turn == "" || o.Generation != prev.Generation || o.Current != "" || o.SessionGone {
		return ""
	}
	later := func(e ObservedTurnEnd) bool { return e.ID != prev.LastID }
	if _, ok := o.recent(turn); ok || slices.ContainsFunc(o.endedAfter(prev.Seen), later) {
		return ""
	}
	return turn
}

// recent returns the ended turn id from Recent.
func (o TurnObservation) recent(id string) (ObservedTurnEnd, bool) {
	i := slices.IndexFunc(o.Recent, func(e ObservedTurnEnd) bool { return e.ID == id })
	if i < 0 {
		return ObservedTurnEnd{}, false
	}
	return o.Recent[i], true
}

// endedAfter returns the ended turns of Recent newer than the anchor seen,
// oldest first. Seen was the newest turn of an earlier Recent, so under the
// caller's contract on Recent it leaves the window only by aging out, when
// all of Recent is newer, or by being current again, when none is.
func (o TurnObservation) endedAfter(seen string) []ObservedTurnEnd {
	n := len(o.Recent)
	switch seen {
	case "":
	case o.Current:
		n = 0
	default:
		if i := slices.IndexFunc(o.Recent, func(e ObservedTurnEnd) bool { return e.ID == seen }); i >= 0 {
			n = i
		}
	}
	out := slices.Clone(o.Recent[:n])
	slices.Reverse(out)
	return out
}

// folder accumulates the facts and cells of one fold.
type folder struct {
	obs          TurnObservation
	interrupting InterruptRef
	next         *TurnCells
	facts        []Fact
	// journaled is the last turn to end before this fold, whose facts are
	// written. It is the only one the fold knows of (see FoldTurn on ends
	// recorded late).
	journaled string
	// ended reports that the fold recorded at least one end.
	ended bool
}

func (f *folder) fact(kind FactKind, turn string, at time.Time, basis FactBasis) Fact {
	return Fact{
		Kind: kind, Conversation: f.obs.Conversation, Generation: f.obs.Generation,
		Turn: turn, Basis: basis, At: at,
	}
}

// endUnseen records turns that started and ended between two observations:
// both facts each, oldest first. A turn the observation also shows current,
// or that is still running, is inconsistent evidence and is skipped.
func (f *folder) endUnseen(turns []ObservedTurnEnd, stillRunning string) {
	for _, e := range turns {
		if e.ID == f.obs.Current || e.ID == stillRunning {
			continue
		}
		if e.ID != f.journaled {
			start := orTime(e.StartedAt, orTime(e.EndedAt, f.obs.ObservedAt))
			f.facts = append(f.facts, f.fact(FactTurnStarted, e.ID, start, FactBasisRuntime))
		}
		f.end(e)
	}
}

// end records e's turn.ended and moves the cells to ended. A turn whose end
// is journaled already moves the cells only.
func (f *folder) end(e ObservedTurnEnd) {
	reason, basis := f.reason(e)
	if e.ID == f.journaled {
		f.setEnded(e.ID, reason)
		return
	}
	fact := f.fact(FactTurnEnded, e.ID, orTime(e.EndedAt, f.obs.ObservedAt), basis)
	fact.Reason, fact.Detail = string(reason), stopDetail(e.StopReason)
	if reason == TurnEndInterrupted {
		fact.CauseOps = []string{f.interrupting.Op}
	}
	f.facts = append(f.facts, fact)
	f.setEnded(e.ID, reason)
}

// endMissing ends the running turn that is neither current nor in Recent. A
// turn whose end is journaled already (one resumed after a false idle) moves
// the cells only.
func (f *folder) endMissing(turn string) {
	reason := TurnEndUnobserved
	if f.obs.SessionGone {
		reason = TurnEndSessionGone
	}
	if turn != f.journaled {
		fact := f.fact(FactTurnEnded, turn, f.obs.ObservedAt, FactBasisObserved)
		fact.Reason = string(reason)
		f.facts = append(f.facts, fact)
	}
	f.setEnded(turn, reason)
}

// setEnded moves the last-end cells to turn, dated by the observer's clock.
// The state cells settle once the whole observation is folded.
func (f *folder) setEnded(turn string, reason TurnEndReason) {
	f.ended = true
	f.next.LastID, f.next.LastEnd, f.next.LastAt = turn, reason, f.obs.ObservedAt
}

// reason maps how the source says e ended to its turn.ended reason and basis.
func (f *folder) reason(e ObservedTurnEnd) (TurnEndReason, FactBasis) {
	byGC := f.interrupting.names(e.ID)
	if e.Inferred {
		if byGC {
			return TurnEndInterrupted, FactBasisObserved
		}
		return TurnEndEnded, FactBasisObserved
	}
	switch e.Status {
	case TurnSourceCompleted:
		return TurnEndCompleted, FactBasisRuntime
	case TurnSourceCancelled:
		if byGC {
			return TurnEndInterrupted, FactBasisRuntime
		}
		return TurnEndInterruptedLocally, FactBasisRuntime
	case TurnSourceFailed:
		if f.obs.SessionGone {
			return TurnEndSessionGone, FactBasisRuntime
		}
		return TurnEndFailed, FactBasisRuntime
	case TurnSourceRestarted:
		return TurnEndRuntimeRestart, FactBasisRuntime
	case "":
		return TurnEndSuperseded, FactBasisRuntime
	default:
		return TurnEndEnded, FactBasisRuntime
	}
}

// stopDetail renders a provider stop reason as a fact Detail: lower-cased,
// and empty when that is not a token.
func stopDetail(stop string) string {
	detail := strings.ToLower(stop)
	if detail == "" || validateToken("stop reason", detail) != nil {
		return ""
	}
	return detail
}

func orTime(t, fallback time.Time) time.Time {
	if t.IsZero() {
		return fallback
	}
	return t
}
