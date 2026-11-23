package opjournal

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
)

// modelTurn is one turn of the simulated source.
type modelTurn struct {
	id      string
	start   time.Time
	end     time.Time
	status  TurnSourceStatus
	running bool
	// unrecorded is the number of reads still to come that miss the turn's
	// end: the source records it that many reads late.
	unrecorded int
	// inferred is an inferred end the observer read for the turn. It stands
	// in Recent until the turn reads current again or its end is recorded.
	inferred bool
}

// turnSourceModel is a seeded source of turn observations that keeps the
// fold's caller contract: Recent is newest first, bounded by window, and an
// ended turn, an inferred end included, stays in Recent until it ages out or
// its turn is current again, even when the session is gone. The source
// records each end late reads after it happens, in order.
type turnSourceModel struct {
	rng     *rand.Rand
	window  int
	late    int
	clock   time.Time
	turns   []*modelTurn
	running *modelTurn
	nextID  int
}

func (m *turnSourceModel) recent() []ObservedTurnEnd {
	var out []ObservedTurnEnd
	for i := len(m.turns) - 1; i >= 0 && len(out) < m.window; i-- {
		switch t := m.turns[i]; {
		case !t.running && t.unrecorded == 0:
			out = append(out, ObservedTurnEnd{ID: t.id, StartedAt: t.start, EndedAt: t.end, Status: t.status})
		case t.inferred:
			out = append(out, ObservedTurnEnd{ID: t.id, StartedAt: t.start, EndedAt: m.clock, Inferred: true})
		}
	}
	return out
}

// read returns Recent and counts the read against every end not yet
// recorded.
func (m *turnSourceModel) read() []ObservedTurnEnd {
	recent := m.recent()
	for _, t := range m.turns {
		if t.unrecorded > 0 {
			t.unrecorded--
		}
	}
	return recent
}

func (m *turnSourceModel) startTurn() {
	m.nextID++
	m.running = &modelTurn{id: fmt.Sprintf("t%d", m.nextID), start: m.clock, running: true}
	m.turns = append(m.turns, m.running)
}

// endTurn ends the running turn with status.
func (m *turnSourceModel) endTurn(status TurnSourceStatus) {
	m.running.running, m.running.end, m.running.status = false, m.clock, status
	m.running.unrecorded = m.late
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
		m.endTurn([]TurnSourceStatus{TurnSourceCompleted, TurnSourceCancelled, TurnSourceFailed}[m.rng.Intn(3)])
		m.running = nil
	case m.running != nil && r < 5:
		m.endTurn("")
		m.startTurn()
	}
}

// observe reads the source. With falseIdle, a running turn may read as
// ended by inference instead of current.
func (m *turnSourceModel) observe(falseIdle bool) TurnObservation {
	obs := TurnObservation{Conversation: "gc-1", Generation: 3, ObservedAt: m.clock, Caps: foldCaps}
	if m.running != nil {
		m.running.inferred = falseIdle && m.rng.Intn(4) == 0
		if !m.running.inferred {
			obs.Current, obs.CurrentStart = m.running.id, m.running.start
		}
	}
	obs.Recent = m.read()
	return obs
}

// goneObservation is the read after the runtime exited: a still-running turn
// is never recorded, and a carried-forward inferred end stays.
func (m *turnSourceModel) goneObservation() TurnObservation {
	m.clock = m.clock.Add(time.Second)
	return TurnObservation{
		Conversation: "gc-1", Generation: 3, ObservedAt: m.clock, Caps: foldCaps,
		Recent: m.read(), SessionGone: true,
	}
}

// TestFoldTurnModel folds seeded runs of a source with gaps, false idles,
// session death and ends recorded late, and writes every fact with
// RecordFact. It checks that every fact encodes, that folding an observation
// again writes nothing, and that the fold returns a journaled fact again only
// when the source records ends late; RecordFact then reports it not created
// and leaves the stored row as it was. A late source must return one.
func TestFoldTurnModel(t *testing.T) {
	// A late source returns journaled facts in most runs, so fewer seeds
	// cover it.
	sources := []struct {
		late  int
		seeds int64
	}{{late: 0, seeds: 2000}, {late: 2, seeds: 500}}
	for _, window := range []int{1, 3, 8} {
		for _, falseIdle := range []bool{false, true} {
			for _, gone := range []bool{false, true} {
				for _, src := range sources {
					late := src.late
					name := fmt.Sprintf("window=%d/false_idle=%v/session_gone=%v/late=%d", window, falseIdle, gone, late)
					t.Run(name, func(t *testing.T) {
						repeats := 0
						for seed := int64(1); seed <= src.seeds; seed++ {
							n, msg := foldTurnModelRun(seed, window, late, falseIdle, gone)
							if msg != "" {
								t.Fatalf("seed %d: %s", seed, msg)
							}
							repeats += n
						}
						if late > 0 && repeats == 0 {
							t.Fatal("no seed returned a journaled fact again")
						}
					})
				}
			}
		}
	}
}

// foldTurnModelRun runs one seed and returns the number of journaled facts
// the fold returned again, and a failure with its trace, or "".
func foldTurnModelRun(seed int64, window, late int, falseIdle, gone bool) (int, string) {
	rng := rand.New(rand.NewSource(seed))
	m := &turnSourceModel{rng: rng, window: window, late: late, clock: t0}
	mem := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	store, err := NewStore(beads.SessionStore{Store: mem}, &clock.Fake{Time: t0}, WithIDPrefix("gc"))
	if err != nil {
		return 0, "NewStore: " + err.Error()
	}
	var cells TurnCells
	recorded := map[string]string{}
	repeats := 0
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
			id, err := FactID("gc", f)
			if err != nil {
				return "fact id: " + err.Error()
			}
			first, journaled := recorded[id]
			if !journaled {
				if created, err := store.RecordFact(f); err != nil || !created {
					return fmt.Sprintf("RecordFact %s = created %v, %v", got[i], created, err)
				}
				recorded[id] = got[i]
				continue
			}
			if late == 0 {
				return fmt.Sprintf("duplicate %s; first %s", got[i], first)
			}
			repeats++
			before, _ := mem.Get(id)
			created, err := store.RecordFact(f)
			if after, _ := mem.Get(id); err != nil || created || !reflect.DeepEqual(after, before) {
				return fmt.Sprintf("RecordFact %s, journaled as %s = created %v, %v; row %+v, was %+v",
					got[i], first, created, err, after, before)
			}
		}
		again, refacts, rewrite := FoldTurn(next, obs, ref)
		if rewrite || len(refacts) > 0 || !again.equal(next) {
			return fmt.Sprintf("refold wrote %v with %d facts", rewrite, len(refacts))
		}
		cells = next
		return ""
	}
	fail := func(msg string) (int, string) { return repeats, msg + "\n" + strings.Join(trace, "\n") }

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
	return repeats, ""
}
