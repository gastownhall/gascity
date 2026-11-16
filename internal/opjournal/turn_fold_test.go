package opjournal

import (
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	ts       = func(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }
	foldCaps = TurnCapsStamp{{"start", "recorded"}, {"end", "recorded"}, {"fence", "checked"}, {"link", "correlated"}, {"source", "transcript"}}
	stopOp   = "gc-op-kdpftlql5feaypldytje3nypwy"
)

func running(gen int64, id string, at time.Time) TurnCells {
	return TurnCells{Generation: gen, Caps: foldCaps, ID: id, State: TurnStateRunning, At: at, ObservedAt: at}
}

// ended is the cells after the end of last, which the source's window
// showed as its newest ended turn.
func ended(last string, reason TurnEndReason, at time.Time) TurnCells {
	return TurnCells{
		Generation: 3, Caps: foldCaps, State: TurnStateEnded, At: at,
		LastID: last, LastEnd: reason, LastAt: at, Seen: last, ObservedAt: at,
	}
}

// with returns c changed by fn.
func with(c TurnCells, fn func(*TurnCells)) TurnCells {
	fn(&c)
	return c
}

func obsAt(gen int64, current string, start time.Time, recent ...ObservedTurnEnd) TurnObservation {
	return TurnObservation{
		Conversation: "gc-1", Generation: gen, Current: current, CurrentStart: start,
		Recent: recent, ObservedAt: ts(100), Caps: foldCaps,
	}
}

func started(gen int64, id string, at time.Time) Fact {
	return Fact{Kind: FactTurnStarted, Conversation: "gc-1", Generation: gen, Turn: id, Basis: FactBasisRuntime, At: at}
}

func endedFact(id string, reason TurnEndReason, basis FactBasis, at time.Time) Fact {
	return Fact{
		Kind: FactTurnEnded, Conversation: "gc-1", Generation: 3, Turn: id,
		Reason: string(reason), Basis: basis, At: at,
	}
}

func TestFoldTurnTable(t *testing.T) {
	cases := []struct {
		name         string
		prev         TurnCells
		obs          TurnObservation
		interrupting InterruptRef
		next         TurnCells
		facts        []Fact
		write        bool
	}{
		{
			name:  "ended, current U: U starts",
			prev:  ended("A", TurnEndCompleted, ts(1)),
			obs:   obsAt(3, "U", ts(5), ObservedTurnEnd{ID: "A", StartedAt: ts(0), EndedAt: ts(1), Status: TurnSourceCompleted}),
			next:  TurnCells{Generation: 3, Caps: foldCaps, ID: "U", State: TurnStateRunning, At: ts(100), LastID: "A", LastEnd: TurnEndCompleted, LastAt: ts(1), Seen: "A", ObservedAt: ts(100)},
			facts: []Fact{started(3, "U", ts(5))},
			write: true,
		},
		{
			name:  "unknown, current U with no source start: U starts at the observation",
			prev:  TurnCells{Generation: 3, State: TurnStateUnknown},
			obs:   obsAt(3, "U", time.Time{}),
			next:  TurnCells{Generation: 3, Caps: foldCaps, ID: "U", State: TurnStateRunning, At: ts(100), ObservedAt: ts(100)},
			facts: []Fact{started(3, "U", ts(100))},
			write: true,
		},
		{
			name:  "running T, current T: nothing",
			prev:  running(3, "T", ts(5)),
			obs:   obsAt(3, "T", ts(5)),
			next:  running(3, "T", ts(5)),
			write: false,
		},
		{
			name: "running T, current U, T in Recent: T ends with its reason, then U starts",
			prev: running(3, "T", ts(5)),
			obs:  obsAt(3, "U", ts(9), ObservedTurnEnd{ID: "T", StartedAt: ts(5), EndedAt: ts(8), Status: TurnSourceCompleted, StopReason: "end_turn"}),
			next: TurnCells{Generation: 3, Caps: foldCaps, ID: "U", State: TurnStateRunning, At: ts(100), LastID: "T", LastEnd: TurnEndCompleted, LastAt: ts(100), Seen: "T", ObservedAt: ts(100)},
			facts: []Fact{
				func() Fact {
					f := endedFact("T", TurnEndCompleted, FactBasisRuntime, ts(8))
					f.Detail = "end_turn"
					return f
				}(),
				started(3, "U", ts(9)),
			},
			write: true,
		},
		{
			name: "running T, current U, T closed by U with no status: superseded",
			prev: running(3, "T", ts(5)),
			obs:  obsAt(3, "U", ts(9), ObservedTurnEnd{ID: "T", StartedAt: ts(5), EndedAt: ts(9)}),
			next: TurnCells{Generation: 3, Caps: foldCaps, ID: "U", State: TurnStateRunning, At: ts(100), LastID: "T", LastEnd: TurnEndSuperseded, LastAt: ts(100), Seen: "T", ObservedAt: ts(100)},
			facts: []Fact{
				endedFact("T", TurnEndSuperseded, FactBasisRuntime, ts(9)),
				started(3, "U", ts(9)),
			},
			write: true,
		},
		{
			name: "running T, current U, T absent from Recent: T unobserved, then U starts",
			prev: running(3, "T", ts(5)),
			obs:  obsAt(3, "U", ts(9)),
			next: TurnCells{Generation: 3, Caps: foldCaps, ID: "U", State: TurnStateRunning, At: ts(100), LastID: "T", LastEnd: TurnEndUnobserved, LastAt: ts(100), ObservedAt: ts(100)},
			facts: []Fact{
				endedFact("T", TurnEndUnobserved, FactBasisObserved, ts(100)),
				started(3, "U", ts(9)),
			},
			write: true,
		},
		{
			name:  "running T, idle, T in Recent: T ends",
			prev:  running(3, "T", ts(5)),
			obs:   obsAt(3, "", time.Time{}, ObservedTurnEnd{ID: "T", StartedAt: ts(5), EndedAt: ts(8), Status: TurnSourceFailed}),
			next:  ended("T", TurnEndFailed, ts(100)),
			facts: []Fact{endedFact("T", TurnEndFailed, FactBasisRuntime, ts(8))},
			write: true,
		},
		{
			name: "running T, idle, T absent: unobserved",
			prev: running(3, "T", ts(5)),
			obs:  obsAt(3, "", time.Time{}),
			next: TurnCells{
				Generation: 3, Caps: foldCaps, State: TurnStateEnded, At: ts(100),
				LastID: "T", LastEnd: TurnEndUnobserved, LastAt: ts(100), ObservedAt: ts(100),
			},
			facts: []Fact{endedFact("T", TurnEndUnobserved, FactBasisObserved, ts(100))},
			write: true,
		},
		{
			name: "running T, the session gone with T absent: session_gone",
			prev: running(3, "T", ts(5)),
			obs: func() TurnObservation {
				o := obsAt(3, "", time.Time{})
				o.SessionGone = true
				return o
			}(),
			next: TurnCells{
				Generation: 3, Caps: foldCaps, State: TurnStateEnded, At: ts(100),
				LastID: "T", LastEnd: TurnEndSessionGone, LastAt: ts(100), ObservedAt: ts(100),
			},
			facts: []Fact{endedFact("T", TurnEndSessionGone, FactBasisObserved, ts(100))},
			write: true,
		},
		{
			name: "turns newer than the last end and never seen running: both facts each, oldest first",
			prev: ended("A", TurnEndCompleted, ts(1)),
			obs: obsAt(3, "D", ts(9),
				ObservedTurnEnd{ID: "C", StartedAt: ts(6), EndedAt: ts(7), Status: TurnSourceCompleted},
				ObservedTurnEnd{ID: "B", StartedAt: ts(3), EndedAt: ts(4), Status: TurnSourceCompleted},
				ObservedTurnEnd{ID: "A", StartedAt: ts(0), EndedAt: ts(1), Status: TurnSourceCompleted},
			),
			next: TurnCells{Generation: 3, Caps: foldCaps, ID: "D", State: TurnStateRunning, At: ts(100), LastID: "C", LastEnd: TurnEndCompleted, LastAt: ts(100), Seen: "C", ObservedAt: ts(100)},
			facts: []Fact{
				started(3, "B", ts(3)), endedFact("B", TurnEndCompleted, FactBasisRuntime, ts(4)),
				started(3, "C", ts(6)), endedFact("C", TurnEndCompleted, FactBasisRuntime, ts(7)),
				started(3, "D", ts(9)),
			},
			write: true,
		},
		{
			name: "running T missed; a turn after T ended unseen: T, then the unseen turn, oldest first",
			prev: running(3, "T", ts(5)),
			obs: obsAt(3, "", time.Time{},
				ObservedTurnEnd{ID: "V", StartedAt: ts(7), EndedAt: ts(8), Status: TurnSourceCompleted},
				ObservedTurnEnd{ID: "T", StartedAt: ts(5), EndedAt: ts(6), Status: TurnSourceCompleted},
			),
			next: ended("V", TurnEndCompleted, ts(100)),
			facts: []Fact{
				endedFact("T", TurnEndCompleted, FactBasisRuntime, ts(6)),
				started(3, "V", ts(7)), endedFact("V", TurnEndCompleted, FactBasisRuntime, ts(8)),
			},
			write: true,
		},
		{
			name: "an unseen end with no source start starts at its end",
			prev: TurnCells{Generation: 3, Caps: foldCaps, State: TurnStateUnknown},
			obs:  obsAt(3, "", time.Time{}, ObservedTurnEnd{ID: "B", EndedAt: ts(4), Status: TurnSourceCompleted}),
			next: ended("B", TurnEndCompleted, ts(100)),
			facts: []Fact{
				started(3, "B", ts(4)), endedFact("B", TurnEndCompleted, FactBasisRuntime, ts(4)),
			},
			write: true,
		},
		{
			name: "a different generation resets the cells; the old open turn is left to session.ended",
			prev: running(3, "T", ts(5)),
			obs: func() TurnObservation {
				o := obsAt(4, "U", ts(50))
				o.Caps = TurnCapsStamp{{"start", "boundary"}}
				return o
			}(),
			next:  TurnCells{Generation: 4, Caps: TurnCapsStamp{{"start", "boundary"}}, ID: "U", State: TurnStateRunning, At: ts(100), ObservedAt: ts(100)},
			facts: []Fact{started(4, "U", ts(50))},
			write: true,
		},
		{
			// Recent holds only the observed generation's turns (TurnObservation's
			// precondition), so after a reset every one of them is new.
			name: "a different generation journals every turn of its Recent",
			prev: ended("A", TurnEndCompleted, ts(1)),
			obs:  obsAt(4, "", time.Time{}, ObservedTurnEnd{ID: "B", StartedAt: ts(60), EndedAt: ts(70), Status: TurnSourceCompleted}),
			next: TurnCells{
				Generation: 4, Caps: foldCaps, State: TurnStateEnded, At: ts(100),
				LastID: "B", LastEnd: TurnEndCompleted, LastAt: ts(100), Seen: "B", ObservedAt: ts(100),
			},
			facts: []Fact{
				started(4, "B", ts(60)),
				func() Fact {
					f := endedFact("B", TurnEndCompleted, FactBasisRuntime, ts(70))
					f.Generation = 4
					return f
				}(),
			},
			write: true,
		},
		{
			name:  "a different generation, idle: reset to unknown",
			prev:  running(3, "T", ts(5)),
			obs:   obsAt(4, "", time.Time{}),
			next:  TurnCells{Generation: 4, Caps: foldCaps, State: TurnStateUnknown, ObservedAt: ts(100)},
			write: true,
		},
		{
			name: "a different generation with no stamp drops the old generation's",
			prev: running(3, "T", ts(5)),
			obs: func() TurnObservation {
				o := obsAt(4, "", time.Time{})
				o.Caps = nil
				return o
			}(),
			next:  TurnCells{Generation: 4, State: TurnStateUnknown, ObservedAt: ts(100)},
			write: true,
		},
		{
			name:  "an older generation is stale: nothing",
			prev:  running(4, "U", ts(50)),
			obs:   obsAt(3, "T", ts(5)),
			next:  running(4, "U", ts(50)),
			write: false,
		},
		{
			name:  "idle, unknown, nothing ended: nothing",
			prev:  TurnCells{Generation: 3, Caps: foldCaps, State: TurnStateUnknown, ObservedAt: ts(1)},
			obs:   obsAt(3, "", time.Time{}),
			next:  TurnCells{Generation: 3, Caps: foldCaps, State: TurnStateUnknown, ObservedAt: ts(1)},
			write: false,
		},
		{
			name:  "ended, the same Recent again: nothing",
			prev:  ended("A", TurnEndCompleted, ts(1)),
			obs:   obsAt(3, "", time.Time{}, ObservedTurnEnd{ID: "A", StartedAt: ts(0), EndedAt: ts(1), Status: TurnSourceCompleted}),
			next:  ended("A", TurnEndCompleted, ts(1)),
			write: false,
		},
		{
			name: "a capability downgrade alone is a write with no facts",
			prev: running(3, "T", ts(5)),
			obs: func() TurnObservation {
				o := obsAt(3, "T", ts(5))
				o.Caps = TurnCapsStamp{{"start", "boundary"}}
				return o
			}(),
			next: func() TurnCells {
				c := running(3, "T", ts(5))
				c.Caps, c.ObservedAt = TurnCapsStamp{{"start", "boundary"}}, ts(100)
				return c
			}(),
			write: true,
		},
		{
			name: "running T, idle, T absent, an older turn in Recent: only T ends",
			prev: with(running(3, "T", ts(5)), func(c *TurnCells) {
				c.LastID, c.LastEnd, c.LastAt, c.Seen = "R", TurnEndCompleted, ts(4), "R"
			}),
			obs: obsAt(3, "", time.Time{}, ObservedTurnEnd{ID: "R", StartedAt: ts(2), EndedAt: ts(4), Status: TurnSourceCompleted}),
			next: TurnCells{
				Generation: 3, Caps: foldCaps, State: TurnStateEnded, At: ts(100),
				LastID: "T", LastEnd: TurnEndUnobserved, LastAt: ts(100), Seen: "R", ObservedAt: ts(100),
			},
			facts: []Fact{endedFact("T", TurnEndUnobserved, FactBasisObserved, ts(100))},
			write: true,
		},
		{
			name: "running T, the session gone, T absent, an older turn in Recent: only T ends",
			prev: with(running(3, "T", ts(5)), func(c *TurnCells) {
				c.LastID, c.LastEnd, c.LastAt, c.Seen = "R", TurnEndCompleted, ts(4), "R"
			}),
			obs: func() TurnObservation {
				o := obsAt(3, "", time.Time{}, ObservedTurnEnd{ID: "R", StartedAt: ts(2), EndedAt: ts(4), Status: TurnSourceCompleted})
				o.SessionGone = true
				return o
			}(),
			next: TurnCells{
				Generation: 3, Caps: foldCaps, State: TurnStateEnded, At: ts(100),
				LastID: "T", LastEnd: TurnEndSessionGone, LastAt: ts(100), Seen: "R", ObservedAt: ts(100),
			},
			facts: []Fact{endedFact("T", TurnEndSessionGone, FactBasisObserved, ts(100))},
			write: true,
		},
		{
			name: "running T, current U, T absent, an older turn in Recent: T unobserved, then U starts",
			prev: with(running(3, "T", ts(5)), func(c *TurnCells) {
				c.LastID, c.LastEnd, c.LastAt, c.Seen = "R", TurnEndCompleted, ts(4), "R"
			}),
			obs: obsAt(3, "U", ts(9), ObservedTurnEnd{ID: "R", StartedAt: ts(2), EndedAt: ts(4), Status: TurnSourceCompleted}),
			next: TurnCells{
				Generation: 3, Caps: foldCaps, ID: "U", State: TurnStateRunning, At: ts(100),
				LastID: "T", LastEnd: TurnEndUnobserved, LastAt: ts(100), Seen: "R", ObservedAt: ts(100),
			},
			facts: []Fact{
				endedFact("T", TurnEndUnobserved, FactBasisObserved, ts(100)),
				started(3, "U", ts(9)),
			},
			write: true,
		},
		{
			name: "the newest seen turn left the window: every turn in Recent is new",
			prev: ended("A", TurnEndCompleted, ts(1)),
			obs: obsAt(3, "", time.Time{},
				ObservedTurnEnd{ID: "C", StartedAt: ts(6), EndedAt: ts(7), Status: TurnSourceCompleted},
				ObservedTurnEnd{ID: "B", StartedAt: ts(3), EndedAt: ts(4), Status: TurnSourceCompleted},
			),
			next: ended("C", TurnEndCompleted, ts(100)),
			facts: []Fact{
				started(3, "B", ts(3)), endedFact("B", TurnEndCompleted, FactBasisRuntime, ts(4)),
				started(3, "C", ts(6)), endedFact("C", TurnEndCompleted, FactBasisRuntime, ts(7)),
			},
			write: true,
		},
		{
			name: "false idle: the inferred-ended turn is current again; the cells go back to running with no fact",
			prev: ended("U", TurnEndEnded, ts(20)),
			obs:  obsAt(3, "U", ts(10), ObservedTurnEnd{ID: "T1", StartedAt: ts(2), EndedAt: ts(4), Status: TurnSourceCompleted}),
			next: TurnCells{
				Generation: 3, Caps: foldCaps, ID: "U", State: TurnStateRunning, At: ts(100),
				LastID: "U", LastEnd: TurnEndEnded, LastAt: ts(20), Seen: "T1", ObservedAt: ts(100),
			},
			write: true,
		},
		{
			name: "the resumed turn ends for real: the cells follow the source and its turn.ended is not written twice",
			prev: TurnCells{
				Generation: 3, Caps: foldCaps, ID: "U", State: TurnStateRunning, At: ts(30),
				LastID: "U", LastEnd: TurnEndEnded, LastAt: ts(20), Seen: "T1", ObservedAt: ts(30),
			},
			obs: obsAt(3, "", time.Time{},
				ObservedTurnEnd{ID: "U", StartedAt: ts(10), EndedAt: ts(40), Status: TurnSourceCompleted},
				ObservedTurnEnd{ID: "T1", StartedAt: ts(2), EndedAt: ts(4), Status: TurnSourceCompleted},
			),
			next:  ended("U", TurnEndCompleted, ts(100)),
			write: true,
		},
		{
			name: "the resumed turn ends with the session gone, absent from Recent: the cells follow and no second turn.ended",
			prev: TurnCells{
				Generation: 3, Caps: foldCaps, ID: "U", State: TurnStateRunning, At: ts(30),
				LastID: "U", LastEnd: TurnEndEnded, LastAt: ts(20), Seen: "T1", ObservedAt: ts(30),
			},
			obs: func() TurnObservation {
				o := obsAt(3, "", time.Time{}, ObservedTurnEnd{ID: "T1", StartedAt: ts(2), EndedAt: ts(4), Status: TurnSourceCompleted})
				o.SessionGone = true
				return o
			}(),
			next: TurnCells{
				Generation: 3, Caps: foldCaps, State: TurnStateEnded, At: ts(100),
				LastID: "U", LastEnd: TurnEndSessionGone, LastAt: ts(100), Seen: "T1", ObservedAt: ts(100),
			},
			write: true,
		},
		{
			name: "the resumed turn ages out of Recent while V runs: the cells follow, V starts, no second turn.ended",
			prev: TurnCells{
				Generation: 3, Caps: foldCaps, ID: "U", State: TurnStateRunning, At: ts(30),
				LastID: "U", LastEnd: TurnEndEnded, LastAt: ts(20), Seen: "T1", ObservedAt: ts(30),
			},
			obs: obsAt(3, "V", ts(50)),
			next: TurnCells{
				Generation: 3, Caps: foldCaps, ID: "V", State: TurnStateRunning, At: ts(100),
				LastID: "U", LastEnd: TurnEndUnobserved, LastAt: ts(100), Seen: "T1", ObservedAt: ts(100),
			},
			facts: []Fact{started(3, "V", ts(50))},
			write: true,
		},
		{
			name: "the session gone with the inferred end carried forward in Recent (the caller's precondition): nothing",
			prev: ended("U", TurnEndEnded, ts(20)),
			obs: func() TurnObservation {
				o := obsAt(3, "", time.Time{},
					ObservedTurnEnd{ID: "U", EndedAt: ts(20), Inferred: true},
					ObservedTurnEnd{ID: "T1", StartedAt: ts(2), EndedAt: ts(4), Status: TurnSourceCompleted},
				)
				o.SessionGone = true
				return o
			}(),
			next:  ended("U", TurnEndEnded, ts(20)),
			write: false,
		},
		{
			name: "a turn ended unobserved that the source later records: no second fact",
			prev: TurnCells{
				Generation: 3, Caps: foldCaps, State: TurnStateEnded, At: ts(30),
				LastID: "T", LastEnd: TurnEndUnobserved, LastAt: ts(30), Seen: "R", ObservedAt: ts(30),
			},
			obs: obsAt(3, "",
				time.Time{},
				ObservedTurnEnd{ID: "T", StartedAt: ts(5), EndedAt: ts(25), Status: TurnSourceCompleted},
				ObservedTurnEnd{ID: "R", StartedAt: ts(2), EndedAt: ts(4), Status: TurnSourceCompleted},
			),
			next:  ended("T", TurnEndCompleted, ts(100)),
			write: true,
		},
		{
			// X and then Y ended unobserved, so X's facts are journaled but
			// LastID is Y. The caller writes them create-if-absent.
			name: "two turns ended unobserved, then recorded: the older one's facts come again",
			prev: TurnCells{
				Generation: 3, Caps: foldCaps, State: TurnStateEnded, At: ts(30),
				LastID: "Y", LastEnd: TurnEndUnobserved, LastAt: ts(30), Seen: "R", ObservedAt: ts(30),
			},
			obs: obsAt(3, "",
				time.Time{},
				ObservedTurnEnd{ID: "Y", StartedAt: ts(20), EndedAt: ts(25), Status: TurnSourceCompleted},
				ObservedTurnEnd{ID: "X", StartedAt: ts(5), EndedAt: ts(15), Status: TurnSourceCompleted},
				ObservedTurnEnd{ID: "R", StartedAt: ts(2), EndedAt: ts(4), Status: TurnSourceCompleted},
			),
			next:  ended("Y", TurnEndCompleted, ts(100)),
			facts: []Fact{started(3, "X", ts(5)), endedFact("X", TurnEndCompleted, FactBasisRuntime, ts(15))},
			write: true,
		},
		{
			// X ended unobserved, then U's inferred end was journaled, and U
			// is running again with X's end not yet recorded, so Seen went
			// back to R. X's recorded end moves LastID back to X, and U's
			// end comes again when U ends.
			name: "a turn recorded late while the later one runs again moves LastID back",
			prev: with(running(3, "U", ts(30)), func(c *TurnCells) {
				c.LastID, c.LastEnd, c.LastAt, c.Seen = "U", TurnEndEnded, ts(25), "R"
			}),
			obs: obsAt(3, "U", ts(20),
				ObservedTurnEnd{ID: "X", StartedAt: ts(5), EndedAt: ts(20)},
				ObservedTurnEnd{ID: "R", StartedAt: ts(2), EndedAt: ts(4), Status: TurnSourceCompleted},
			),
			next: with(running(3, "U", ts(30)), func(c *TurnCells) {
				c.LastID, c.LastEnd, c.LastAt, c.Seen, c.ObservedAt = "X", TurnEndSuperseded, ts(100), "X", ts(100)
			}),
			facts: []Fact{started(3, "X", ts(5)), endedFact("X", TurnEndSuperseded, FactBasisRuntime, ts(20))},
			write: true,
		},
		{
			name: "no capabilities in the observation keep the stored ones",
			prev: running(3, "T", ts(5)),
			obs: func() TurnObservation {
				o := obsAt(3, "T", ts(5))
				o.Caps = nil
				return o
			}(),
			next:  running(3, "T", ts(5)),
			write: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			next, facts, write := FoldTurn(c.prev, c.obs, c.interrupting)
			if write != c.write {
				t.Fatalf("write = %v, want %v", write, c.write)
			}
			if !reflect.DeepEqual(next, c.next) {
				t.Fatalf("next\n got %+v\nwant %+v", next, c.next)
			}
			if !reflect.DeepEqual(facts, c.facts) {
				t.Fatalf("facts\n got %+v\nwant %+v", facts, c.facts)
			}
			for _, f := range facts {
				if _, err := EncodeFact("gc", f); err != nil {
					t.Fatalf("fact %+v does not encode: %v", f, err)
				}
			}
			// An unchanged state writes nothing: the same observation folded
			// again over the cells it produced is no write and no facts.
			again, refacts, rewrite := FoldTurn(next, c.obs, c.interrupting)
			if rewrite || refacts != nil || !reflect.DeepEqual(again, next) {
				t.Fatalf("folding the same observation again: write %v, facts %+v, cells %+v", rewrite, refacts, again)
			}
		})
	}
}

func TestFoldTurnReasonMapping(t *testing.T) {
	other := InterruptRef{Op: stopOp, Turn: "other"}
	this := InterruptRef{Op: stopOp, Turn: "T"}
	cases := []struct {
		name         string
		end          ObservedTurnEnd
		gone         bool
		interrupting InterruptRef
		reason       TurnEndReason
		basis        FactBasis
		causes       []string
	}{
		{"completed", ObservedTurnEnd{Status: TurnSourceCompleted}, false, this, TurnEndCompleted, FactBasisRuntime, nil},
		{"canceled by gc", ObservedTurnEnd{Status: TurnSourceCancelled}, false, this, TurnEndInterrupted, FactBasisRuntime, []string{stopOp}},
		{"canceled, gc interrupting another turn", ObservedTurnEnd{Status: TurnSourceCancelled}, false, other, TurnEndInterruptedLocally, FactBasisRuntime, nil},
		{"canceled, gc not interrupting", ObservedTurnEnd{Status: TurnSourceCancelled}, false, InterruptRef{}, TurnEndInterruptedLocally, FactBasisRuntime, nil},
		{"failed", ObservedTurnEnd{Status: TurnSourceFailed}, false, InterruptRef{}, TurnEndFailed, FactBasisRuntime, nil},
		{"failed with the session gone", ObservedTurnEnd{Status: TurnSourceFailed}, true, InterruptRef{}, TurnEndSessionGone, FactBasisRuntime, nil},
		{"restarted", ObservedTurnEnd{Status: TurnSourceRestarted}, false, InterruptRef{}, TurnEndRuntimeRestart, FactBasisRuntime, nil},
		{"no status: superseded", ObservedTurnEnd{}, false, this, TurnEndSuperseded, FactBasisRuntime, nil},
		{"inferred while gc interrupts it", ObservedTurnEnd{Inferred: true}, false, this, TurnEndInterrupted, FactBasisObserved, []string{stopOp}},
		{"inferred, gc interrupting another turn", ObservedTurnEnd{Inferred: true}, false, other, TurnEndEnded, FactBasisObserved, nil},
		{"inferred", ObservedTurnEnd{Inferred: true}, false, InterruptRef{}, TurnEndEnded, FactBasisObserved, nil},
		{"a status this build does not know", ObservedTurnEnd{Status: "paused"}, false, InterruptRef{}, TurnEndEnded, FactBasisRuntime, nil},
		// interrupted names the op that caused it (X11 D5): a ref with no op
		// is no gc interrupt.
		{"canceled, a ref with no op id", ObservedTurnEnd{Status: TurnSourceCancelled}, false, InterruptRef{Turn: "T"}, TurnEndInterruptedLocally, FactBasisRuntime, nil},
		{"inferred, a ref with no op id", ObservedTurnEnd{Inferred: true}, false, InterruptRef{Turn: "T"}, TurnEndEnded, FactBasisObserved, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			end := c.end
			end.ID, end.StartedAt, end.EndedAt, end.StopReason = "T", ts(5), ts(8), "end_turn"
			obs := obsAt(3, "", time.Time{}, end)
			obs.SessionGone = c.gone
			next, facts, write := FoldTurn(running(3, "T", ts(5)), obs, c.interrupting)
			if !write || len(facts) != 1 {
				t.Fatalf("write %v, facts %+v", write, facts)
			}
			wantAt := ts(8)
			want := endedFact("T", c.reason, c.basis, wantAt)
			want.Detail, want.CauseOps = "end_turn", c.causes
			if !reflect.DeepEqual(facts[0], want) {
				t.Fatalf("fact\n got %+v\nwant %+v", facts[0], want)
			}
			if next.LastEnd != c.reason || next.State != TurnStateEnded || next.LastID != "T" {
				t.Fatalf("cells %+v", next)
			}
			if _, err := EncodeFact("gc", facts[0]); err != nil {
				t.Fatalf("fact does not encode: %v", err)
			}
		})
	}
}

func TestTurnEndReasons(t *testing.T) {
	want := []TurnEndReason{
		"completed", "interrupted", "interrupted_locally", "failed", "superseded",
		"ended", "session_gone", "runtime_restart", "unobserved",
	}
	if got := TurnEndReasons(); !reflect.DeepEqual(got, want) {
		t.Fatalf("TurnEndReasons() = %v, want %v", got, want)
	}
	for _, r := range want {
		if err := validateToken("reason", string(r)); err != nil {
			t.Errorf("reason %s is not an OJ-1 token: %v", r, err)
		}
	}
}

// The fold is pure: its file imports the standard library only, so no
// runtime, session or I/O package can reach it.
func TestTurnFoldIsStdlibOnly(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "turn_fold.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing turn_fold.go: %v", err)
	}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") {
			t.Errorf("turn_fold.go imports %s", path)
		}
	}
}

// A provider stop reason becomes the fact's Detail only as a token, so a
// reason outside the alphabet never makes the end's fact unencodable.
func TestFoldTurnStopReasonDetail(t *testing.T) {
	cases := []struct{ stop, detail string }{
		{"end_turn", "end_turn"},
		{"MAX_TOKENS", "max_tokens"},
		{"stop sequence", ""},
		{"refusal!", ""},
		{strings.Repeat("a", maxTokenLen+1), ""},
		{"", ""},
	}
	for _, c := range cases {
		t.Run(c.stop, func(t *testing.T) {
			obs := obsAt(3, "", time.Time{}, ObservedTurnEnd{ID: "T", EndedAt: ts(8), Status: TurnSourceCompleted, StopReason: c.stop})
			_, facts, _ := FoldTurn(running(3, "T", ts(5)), obs, InterruptRef{})
			if len(facts) != 1 || facts[0].Detail != c.detail {
				t.Fatalf("facts %+v, want one with detail %q", facts, c.detail)
			}
			if _, err := EncodeFact("gc", facts[0]); err != nil {
				t.Fatalf("fact does not encode: %v", err)
			}
		})
	}
}

// inferIdle is o for a source whose ends may be inferred at idle (T3 V1).
func inferIdle(o TurnObservation) TurnObservation {
	o.InferIdleEnds = true
	return o
}

// afterInferredR is c once R's end was inferred at idle: R is the turn
// journaled last, and Seen, which an inferred end never moves, does not name
// it.
func afterInferredR(c TurnCells) TurnCells {
	c.LastID, c.LastEnd, c.LastAt = "R", TurnEndEnded, ts(4)
	return c
}

// Kills a fold that reads a source's normal unrecorded end (T3 V1 clears
// latestTurn at most turn ends: running T, then idle with T missing from
// Recent) as a gap, or that reads a real gap on such a source as an end.
func TestFoldTurnInfersIdleEnds(t *testing.T) {
	known := func(c TurnCells) TurnCells {
		c.LastID, c.LastEnd, c.LastAt, c.Seen = "R", TurnEndCompleted, ts(4), "R"
		return c
	}
	olderR := ObservedTurnEnd{ID: "R", StartedAt: ts(2), EndedAt: ts(4), Status: TurnSourceCompleted}
	cases := []struct {
		name         string
		prev         TurnCells
		obs          TurnObservation
		interrupting InterruptRef
		next         TurnCells
		facts        []Fact
	}{
		{
			name: "running T, idle, T absent: an inferred end",
			prev: running(3, "T", ts(5)),
			obs:  inferIdle(obsAt(3, "", time.Time{})),
			next: TurnCells{
				Generation: 3, Caps: foldCaps, State: TurnStateEnded, At: ts(100),
				LastID: "T", LastEnd: TurnEndEnded, LastAt: ts(100), ObservedAt: ts(100),
			},
			facts: []Fact{endedFact("T", TurnEndEnded, FactBasisObserved, ts(100))},
		},
		{
			name: "running T, idle, T absent, an older known turn in Recent: an inferred end",
			prev: known(running(3, "T", ts(5))),
			obs:  inferIdle(obsAt(3, "", time.Time{}, olderR)),
			next: TurnCells{
				Generation: 3, Caps: foldCaps, State: TurnStateEnded, At: ts(100),
				LastID: "T", LastEnd: TurnEndEnded, LastAt: ts(100), Seen: "R", ObservedAt: ts(100),
			},
			facts: []Fact{endedFact("T", TurnEndEnded, FactBasisObserved, ts(100))},
		},
		{
			name: "running T, idle, T absent, the inferred R restored after T ran: an inferred end, the cells left on T",
			prev: afterInferredR(running(3, "T", ts(5))),
			obs:  inferIdle(obsAt(3, "", time.Time{}, olderR)),
			next: TurnCells{
				Generation: 3, Caps: foldCaps, State: TurnStateEnded, At: ts(100),
				LastID: "T", LastEnd: TurnEndEnded, LastAt: ts(100), Seen: "R", ObservedAt: ts(100),
			},
			facts: []Fact{endedFact("T", TurnEndEnded, FactBasisObserved, ts(100))},
		},
		{
			name: "running T, idle, T absent, a later turn past the inferred R in Recent: still a gap",
			prev: afterInferredR(running(3, "T", ts(5))),
			obs:  inferIdle(obsAt(3, "", time.Time{}, ObservedTurnEnd{ID: "U", StartedAt: ts(9), EndedAt: ts(12), Status: TurnSourceCompleted}, olderR)),
			next: TurnCells{
				Generation: 3, Caps: foldCaps, State: TurnStateEnded, At: ts(100),
				LastID: "U", LastEnd: TurnEndCompleted, LastAt: ts(100), Seen: "U", ObservedAt: ts(100),
			},
			facts: []Fact{
				endedFact("T", TurnEndUnobserved, FactBasisObserved, ts(100)),
				started(3, "U", ts(9)),
				endedFact("U", TurnEndCompleted, FactBasisRuntime, ts(12)),
			},
		},
		{
			name:         "running T, idle, T absent, gc interrupting T: interrupted",
			prev:         running(3, "T", ts(5)),
			obs:          inferIdle(obsAt(3, "", time.Time{})),
			interrupting: InterruptRef{Op: stopOp, Turn: "T"},
			next: TurnCells{
				Generation: 3, Caps: foldCaps, State: TurnStateEnded, At: ts(100),
				LastID: "T", LastEnd: TurnEndInterrupted, LastAt: ts(100), ObservedAt: ts(100),
			},
			facts: []Fact{func() Fact {
				f := endedFact("T", TurnEndInterrupted, FactBasisObserved, ts(100))
				f.CauseOps = []string{stopOp}
				return f
			}()},
		},
		{
			name: "running T, current U, T absent: still a gap",
			prev: running(3, "T", ts(5)),
			obs:  inferIdle(obsAt(3, "U", ts(9))),
			next: TurnCells{Generation: 3, Caps: foldCaps, ID: "U", State: TurnStateRunning, At: ts(100), LastID: "T", LastEnd: TurnEndUnobserved, LastAt: ts(100), ObservedAt: ts(100)},
			facts: []Fact{
				endedFact("T", TurnEndUnobserved, FactBasisObserved, ts(100)),
				started(3, "U", ts(9)),
			},
		},
		{
			name: "running T, idle, T absent, an unrelated later turn in Recent: still a gap",
			prev: known(running(3, "T", ts(5))),
			obs:  inferIdle(obsAt(3, "", time.Time{}, ObservedTurnEnd{ID: "U", StartedAt: ts(9), EndedAt: ts(12), Status: TurnSourceCompleted})),
			next: TurnCells{
				Generation: 3, Caps: foldCaps, State: TurnStateEnded, At: ts(100),
				LastID: "U", LastEnd: TurnEndCompleted, LastAt: ts(100), Seen: "U", ObservedAt: ts(100),
			},
			facts: []Fact{
				endedFact("T", TurnEndUnobserved, FactBasisObserved, ts(100)),
				started(3, "U", ts(9)),
				endedFact("U", TurnEndCompleted, FactBasisRuntime, ts(12)),
			},
		},
		{
			name: "running T, the session gone with T absent: session_gone",
			prev: running(3, "T", ts(5)),
			obs: func() TurnObservation {
				o := inferIdle(obsAt(3, "", time.Time{}))
				o.SessionGone = true
				return o
			}(),
			next: TurnCells{
				Generation: 3, Caps: foldCaps, State: TurnStateEnded, At: ts(100),
				LastID: "T", LastEnd: TurnEndSessionGone, LastAt: ts(100), ObservedAt: ts(100),
			},
			facts: []Fact{endedFact("T", TurnEndSessionGone, FactBasisObserved, ts(100))},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			next, facts, write := FoldTurn(c.prev, c.obs, c.interrupting)
			if !write {
				t.Fatal("write = false, want true")
			}
			if !reflect.DeepEqual(next, c.next) {
				t.Fatalf("next\n got %+v\nwant %+v", next, c.next)
			}
			if !reflect.DeepEqual(facts, c.facts) {
				t.Fatalf("facts\n got %+v\nwant %+v", facts, c.facts)
			}
			for _, f := range facts {
				if _, err := EncodeFact("gc", f); err != nil {
					t.Fatalf("fact %+v does not encode: %v", f, err)
				}
			}
			again, refacts, rewrite := FoldTurn(next, c.obs, c.interrupting)
			if rewrite || refacts != nil || !reflect.DeepEqual(again, next) {
				t.Fatalf("folding the same observation again: write %v, facts %+v, cells %+v", rewrite, refacts, again)
			}
		})
	}
}

// Kills an InferredIdleEnd that disagrees with the fold: it names the
// running turn exactly when the fold infers that turn's end at idle.
func TestInferredIdleEndMatchesTheFold(t *testing.T) {
	later := ObservedTurnEnd{ID: "U", StartedAt: ts(9), EndedAt: ts(12), Status: TurnSourceCompleted}
	restoredR := ObservedTurnEnd{ID: "R", StartedAt: ts(2), EndedAt: ts(4), Status: TurnSourceCompleted}
	cases := []struct {
		name string
		prev TurnCells
		obs  TurnObservation
		want string
	}{
		{"idle, T absent", running(3, "T", ts(5)), inferIdle(obsAt(3, "", time.Time{})), "T"},
		{"the inferred R restored to Recent", afterInferredR(running(3, "T", ts(5))), inferIdle(obsAt(3, "", time.Time{}, restoredR)), "T"},
		{"a later turn past the restored R", afterInferredR(running(3, "T", ts(5))), inferIdle(obsAt(3, "", time.Time{}, later, restoredR)), ""},
		{"the source records every end", running(3, "T", ts(5)), obsAt(3, "", time.Time{}), ""},
		{"T current", running(3, "T", ts(5)), inferIdle(obsAt(3, "T", ts(5))), ""},
		{"another turn current", running(3, "T", ts(5)), inferIdle(obsAt(3, "U", ts(9))), ""},
		{"T in Recent", running(3, "T", ts(5)), inferIdle(obsAt(3, "", time.Time{}, ObservedTurnEnd{ID: "T", Status: TurnSourceCompleted})), ""},
		{"a later turn in Recent", running(3, "T", ts(5)), inferIdle(obsAt(3, "", time.Time{}, later)), ""},
		{"nothing running", ended("R", TurnEndCompleted, ts(4)), inferIdle(obsAt(3, "", time.Time{})), ""},
		{"another generation", running(2, "T", ts(5)), inferIdle(obsAt(3, "", time.Time{})), ""},
		{"the session gone", running(3, "T", ts(5)), func() TurnObservation {
			o := inferIdle(obsAt(3, "", time.Time{}))
			o.SessionGone = true
			return o
		}(), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.obs.InferredIdleEnd(c.prev); got != c.want {
				t.Fatalf("InferredIdleEnd = %q, want %q", got, c.want)
			}
			_, facts, _ := FoldTurn(c.prev, c.obs, InterruptRef{})
			inferred := ""
			for _, f := range facts {
				if f.Kind == FactTurnEnded && f.Turn == c.prev.ID && f.Reason == string(TurnEndEnded) {
					inferred = f.Turn
				}
			}
			if inferred != c.want {
				t.Fatalf("the fold inferred the end of %q, want %q", inferred, c.want)
			}
		})
	}
}
