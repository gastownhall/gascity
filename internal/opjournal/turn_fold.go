package opjournal

import (
	"slices"
	"time"
)

// TurnState is the state of the turn cells (gc.rt.turn.state). The set is
// closed. The empty value is a row whose observer has not yet written the
// cells; readers treat it as TurnStateUnknown.
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
// every downgrade; the codec does not interpret it. The typed capability axes
// the lane reads (DES-O1 §4.2's Caps.Turns) are parsed from it at that edge
// and are not this type.
type TurnCapsStamp []TurnCapsPair

// TurnCells are the runtime row's gc.rt.turn.* cells. The turn observer is
// their only writer (D-TURN). They are observations: hints with an age, never
// proof that a turn is or is not running. ObservedAt is the age anchor, when
// the observer last saw a transition; At and LastAt are the source's times
// when it gave them.
type TurnCells struct {
	// Generation is the session the cells describe (gc.rt.turn.gen). A
	// reader whose current generation is another reads every cell as
	// unknown.
	Generation int64
	// Caps is the capabilities stamp (gc.rt.turn.caps).
	Caps TurnCapsStamp
	// ID is the running turn's source-owned id while State is running, and
	// empty otherwise (gc.rt.turn.id).
	ID string
	// State is gc.rt.turn.state.
	State TurnState
	// At is when the current state began: the running turn's start, or the
	// last end (gc.rt.turn.at).
	At time.Time
	// LastID, LastEnd and LastAt name the last turn seen to end, its reason
	// and its end time (gc.rt.turn.last_id, last_end, last_at).
	LastID  string
	LastEnd TurnEndReason
	LastAt  time.Time
	// ObservedAt is the observer's clock at the observation that last
	// changed the cells (gc.rt.turn.observed_at).
	ObservedAt time.Time
}

func (c TurnCells) equal(o TurnCells) bool {
	return c.Generation == o.Generation && slices.Equal(c.Caps, o.Caps) && c.ID == o.ID &&
		c.State == o.State && c.At.Equal(o.At) && c.LastID == o.LastID && c.LastEnd == o.LastEnd &&
		c.LastAt.Equal(o.LastAt) && c.ObservedAt.Equal(o.ObservedAt)
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
	// StopReason is the provider's own stop reason, if any; it becomes the
	// fact's Detail.
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
	// source's bound.
	Recent []ObservedTurnEnd
	// SessionGone reports that the runtime exited.
	SessionGone bool
	// ObservedAt is the observer's clock at the read. It is required: it
	// dates every cell write and every fact without a source time.
	ObservedAt time.Time
	// Caps is the capabilities stamp to publish; nil keeps the stored one.
	Caps TurnCapsStamp
}

// InterruptRef names the interrupt gc is running on a turn, so the fold
// credits that turn's end to the op. The zero value is no interrupt.
type InterruptRef struct {
	Op   string
	Turn string
}

// names reports whether r is an interrupt of turn.
func (r InterruptRef) names(turn string) bool { return r.Turn != "" && r.Turn == turn }

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
//     when the session is gone; else unobserved.
//   - Turns in Recent newer than the last known end and never seen running
//     get both facts, oldest first.
//   - A current turn that is not the running one starts.
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
	f := folder{obs: obs, interrupting: interrupting, next: &next}

	runningTurn := next.running()
	unseen := obs.newerThan(next.LastID)
	if runningTurn != "" && obs.Current != runningTurn {
		if i := slices.IndexFunc(unseen, func(e ObservedTurnEnd) bool { return e.ID == runningTurn }); i >= 0 {
			// Recorded: its end comes in order with the other unseen turns.
			f.endUnseen(unseen[:i], runningTurn)
			f.end(unseen[i])
			unseen = unseen[i+1:]
		} else if e, ok := obs.recent(runningTurn); ok {
			f.end(e)
		} else {
			f.endMissing(runningTurn)
		}
		runningTurn = ""
	}
	f.endUnseen(unseen, runningTurn)

	switch {
	case obs.Current != "" && obs.Current != runningTurn:
		at := orTime(obs.CurrentStart, obs.ObservedAt)
		f.facts = append(f.facts, f.fact(FactTurnStarted, obs.Current, at, FactBasisRuntime))
		next.ID, next.State, next.At = obs.Current, TurnStateRunning, at
	case obs.Current == "" && f.ended:
		next.ID, next.State, next.At = "", TurnStateEnded, next.LastAt
	}

	if len(f.facts) == 0 && next.equal(prev) {
		return prev, nil, false
	}
	next.ObservedAt = obs.ObservedAt
	return next, f.facts, true
}

// recent returns the ended turn id from Recent.
func (o TurnObservation) recent(id string) (ObservedTurnEnd, bool) {
	i := slices.IndexFunc(o.Recent, func(e ObservedTurnEnd) bool { return e.ID == id })
	if i < 0 {
		return ObservedTurnEnd{}, false
	}
	return o.Recent[i], true
}

// newerThan returns the ended turns of Recent after lastID, oldest first:
// all of Recent when lastID is empty or has left the window.
func (o TurnObservation) newerThan(lastID string) []ObservedTurnEnd {
	n := len(o.Recent)
	if lastID != "" {
		if i := slices.IndexFunc(o.Recent, func(e ObservedTurnEnd) bool { return e.ID == lastID }); i >= 0 {
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
		start := orTime(e.StartedAt, orTime(e.EndedAt, f.obs.ObservedAt))
		f.facts = append(f.facts, f.fact(FactTurnStarted, e.ID, start, FactBasisRuntime))
		f.end(e)
	}
}

// end records e's turn.ended and moves the cells to ended.
func (f *folder) end(e ObservedTurnEnd) {
	reason, basis := f.reason(e)
	fact := f.fact(FactTurnEnded, e.ID, orTime(e.EndedAt, f.obs.ObservedAt), basis)
	fact.Reason, fact.Detail = string(reason), e.StopReason
	if reason == TurnEndInterrupted && f.interrupting.Op != "" {
		fact.CauseOps = []string{f.interrupting.Op}
	}
	f.facts = append(f.facts, fact)
	f.setEnded(e.ID, reason, fact.At)
}

// endMissing ends the running turn that is neither current nor in Recent.
func (f *folder) endMissing(turn string) {
	reason := TurnEndUnobserved
	if f.obs.SessionGone {
		reason = TurnEndSessionGone
	}
	fact := f.fact(FactTurnEnded, turn, f.obs.ObservedAt, FactBasisObserved)
	fact.Reason = string(reason)
	f.facts = append(f.facts, fact)
	f.setEnded(turn, reason, fact.At)
}

// setEnded moves the last-end cells to turn. The state cells settle once the
// whole observation is folded.
func (f *folder) setEnded(turn string, reason TurnEndReason, at time.Time) {
	f.ended = true
	f.next.LastID, f.next.LastEnd, f.next.LastAt = turn, reason, at
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

func orTime(t, fallback time.Time) time.Time {
	if t.IsZero() {
		return fallback
	}
	return t
}
