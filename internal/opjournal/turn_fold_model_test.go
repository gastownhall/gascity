package opjournal

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// modelTurn is one turn of the simulated source.
type modelTurn struct {
	id      string
	start   time.Time
	end     time.Time
	status  TurnSourceStatus
	running bool
}

// turnSourceModel is a seeded source of turn observations that keeps the
// fold's caller contract: Recent is newest first, bounded by window, and an
// inferred end stays in Recent until it ages out or its turn is current
// again, even when the session is gone.
type turnSourceModel struct {
	rng      *rand.Rand
	window   int
	clock    time.Time
	turns    []*modelTurn
	running  *modelTurn
	inferred string
	nextID   int
}

func (m *turnSourceModel) recent() []ObservedTurnEnd {
	var out []ObservedTurnEnd
	if m.inferred != "" {
		out = append(out, ObservedTurnEnd{ID: m.running.id, StartedAt: m.running.start, EndedAt: m.clock, Inferred: true})
	}
	for i := len(m.turns) - 1; i >= 0 && len(out) < m.window; i-- {
		if t := m.turns[i]; !t.running {
			out = append(out, ObservedTurnEnd{ID: t.id, StartedAt: t.start, EndedAt: t.end, Status: t.status})
		}
	}
	return out
}

func (m *turnSourceModel) startTurn() {
	m.nextID++
	m.running = &modelTurn{id: fmt.Sprintf("t%d", m.nextID), start: m.clock, running: true}
	m.turns = append(m.turns, m.running)
}

// advance moves the source one tick: a turn may start, end with a recorded
// status, or be closed by the next turn with no status.
func (m *turnSourceModel) advance() {
	m.clock = m.clock.Add(time.Second)
	r := m.rng.Intn(10)
	switch {
	case m.running == nil && r < 6:
		m.startTurn()
	case m.running != nil && r < 3:
		m.running.running, m.running.end = false, m.clock
		m.running.status = []TurnSourceStatus{TurnSourceCompleted, TurnSourceCancelled, TurnSourceFailed}[m.rng.Intn(3)]
		m.running, m.inferred = nil, ""
	case m.running != nil && r < 5:
		m.running.running, m.running.end, m.running.status = false, m.clock, ""
		m.inferred = ""
		m.startTurn()
	}
}

// observe reads the source. With falseIdle, a running turn may read as
// ended by inference instead of current.
func (m *turnSourceModel) observe(falseIdle bool) TurnObservation {
	obs := TurnObservation{Conversation: "gc-1", Generation: 3, ObservedAt: m.clock, Caps: foldCaps}
	m.inferred = ""
	if m.running != nil {
		if falseIdle && m.rng.Intn(4) == 0 {
			m.inferred = m.running.id
		} else {
			obs.Current, obs.CurrentStart = m.running.id, m.running.start
		}
	}
	obs.Recent = m.recent()
	return obs
}

// goneObservation is the read after the runtime exited: a still-running turn
// is never recorded, and a carried-forward inferred end stays.
func (m *turnSourceModel) goneObservation() TurnObservation {
	m.clock = m.clock.Add(time.Second)
	return TurnObservation{
		Conversation: "gc-1", Generation: 3, ObservedAt: m.clock, Caps: foldCaps,
		Recent: m.recent(), SessionGone: true,
	}
}

// TestFoldTurnModel folds seeded runs of a source with gaps, false idles and
// session death, and checks that the fold never journals one fact twice,
// that every fact encodes, and that folding an observation again writes
// nothing.
func TestFoldTurnModel(t *testing.T) {
	const seeds = 2000
	for _, window := range []int{1, 3, 8} {
		for _, falseIdle := range []bool{false, true} {
			for _, gone := range []bool{false, true} {
				name := fmt.Sprintf("window=%d/false_idle=%v/session_gone=%v", window, falseIdle, gone)
				t.Run(name, func(t *testing.T) {
					for seed := int64(1); seed <= seeds; seed++ {
						if msg := foldTurnModelRun(seed, window, falseIdle, gone); msg != "" {
							t.Fatalf("seed %d: %s", seed, msg)
						}
					}
				})
			}
		}
	}
}

// foldTurnModelRun runs one seed and returns a failure with its trace, or "".
func foldTurnModelRun(seed int64, window int, falseIdle, gone bool) string {
	rng := rand.New(rand.NewSource(seed))
	m := &turnSourceModel{rng: rng, window: window, clock: t0}
	var cells TurnCells
	recorded := map[string]string{}
	var trace []string
	fold := func(obs TurnObservation, ref InterruptRef) string {
		next, facts, write := FoldTurn(cells, obs, ref)
		var got []string
		for _, f := range facts {
			got = append(got, fmt.Sprintf("%s{%s,%s}", f.Kind, f.Turn, f.Reason))
		}
		trace = append(trace, fmt.Sprintf("cur=%q recent=%d gone=%v -> write=%v facts=%v cells{%s %s last=%s/%s seen=%s}",
			obs.Current, len(obs.Recent), obs.SessionGone, write, got, next.State, next.ID, next.LastID, next.LastEnd, next.Seen))
		for i, f := range facts {
			if _, err := EncodeFact("gc", f); err != nil {
				return "encode: " + err.Error()
			}
			id, err := FactID("gc", f)
			if err != nil {
				return "fact id: " + err.Error()
			}
			if first, dup := recorded[id]; dup {
				return fmt.Sprintf("duplicate %s; first %s", got[i], first)
			}
			recorded[id] = got[i]
		}
		again, refacts, rewrite := FoldTurn(next, obs, ref)
		if rewrite || len(refacts) > 0 || !again.equal(next) {
			return fmt.Sprintf("refold wrote %v with %d facts", rewrite, len(refacts))
		}
		cells = next
		return ""
	}
	fail := func(msg string) string { return msg + "\n" + strings.Join(trace, "\n") }

	steps := 5 + rng.Intn(40)
	for k := 0; k < steps; k++ {
		m.advance()
		if rng.Intn(3) == 0 {
			continue // a gap: the observer misses this tick
		}
		obs := m.observe(falseIdle)
		var ref InterruptRef
		if m.running != nil && rng.Intn(4) == 0 {
			ref = InterruptRef{Op: stopOp, Turn: m.running.id}
		}
		if msg := fold(obs, ref); msg != "" {
			return fail(msg)
		}
	}
	if gone {
		if msg := fold(m.goneObservation(), InterruptRef{}); msg != "" {
			return fail(msg)
		}
	}
	return ""
}
