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
	foldCaps = TurnCaps{{"start", "recorded"}, {"end", "recorded"}, {"fence", "checked"}, {"link", "correlated"}, {"source", "transcript"}}
	stopOp   = "gc-op-kdpftlql5feaypldytje3nypwy"
)

func running(gen int64, id string, at time.Time) TurnCells {
	return TurnCells{Generation: gen, Caps: foldCaps, ID: id, State: TurnStateRunning, At: at, ObservedAt: at}
}

func ended(last string, reason TurnEndReason, at time.Time) TurnCells {
	return TurnCells{
		Generation: 3, Caps: foldCaps, State: TurnStateEnded, At: at,
		LastID: last, LastEnd: reason, LastAt: at, ObservedAt: at,
	}
}

// seen is c as the observation at ts(100) wrote it.
func seen(c TurnCells) TurnCells {
	c.ObservedAt = ts(100)
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
			next:  TurnCells{Generation: 3, Caps: foldCaps, ID: "U", State: TurnStateRunning, At: ts(5), LastID: "A", LastEnd: TurnEndCompleted, LastAt: ts(1), ObservedAt: ts(100)},
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
			next: TurnCells{Generation: 3, Caps: foldCaps, ID: "U", State: TurnStateRunning, At: ts(9), LastID: "T", LastEnd: TurnEndCompleted, LastAt: ts(8), ObservedAt: ts(100)},
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
			next: TurnCells{Generation: 3, Caps: foldCaps, ID: "U", State: TurnStateRunning, At: ts(9), LastID: "T", LastEnd: TurnEndSuperseded, LastAt: ts(9), ObservedAt: ts(100)},
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
			next: TurnCells{Generation: 3, Caps: foldCaps, ID: "U", State: TurnStateRunning, At: ts(9), LastID: "T", LastEnd: TurnEndUnobserved, LastAt: ts(100), ObservedAt: ts(100)},
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
			next:  seen(ended("T", TurnEndFailed, ts(8))),
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
			next: TurnCells{Generation: 3, Caps: foldCaps, ID: "D", State: TurnStateRunning, At: ts(9), LastID: "C", LastEnd: TurnEndCompleted, LastAt: ts(7), ObservedAt: ts(100)},
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
			next: seen(ended("V", TurnEndCompleted, ts(8))),
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
			next: seen(ended("B", TurnEndCompleted, ts(4))),
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
				o.Caps = TurnCaps{{"start", "boundary"}}
				return o
			}(),
			next:  TurnCells{Generation: 4, Caps: TurnCaps{{"start", "boundary"}}, ID: "U", State: TurnStateRunning, At: ts(50), ObservedAt: ts(100)},
			facts: []Fact{started(4, "U", ts(50))},
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
				o.Caps = TurnCaps{{"start", "boundary"}}
				return o
			}(),
			next: func() TurnCells {
				c := running(3, "T", ts(5))
				c.Caps, c.ObservedAt = TurnCaps{{"start", "boundary"}}, ts(100)
				return c
			}(),
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
		{"an interrupt with no op id", ObservedTurnEnd{Status: TurnSourceCancelled}, false, InterruptRef{Turn: "T"}, TurnEndInterrupted, FactBasisRuntime, nil},
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
