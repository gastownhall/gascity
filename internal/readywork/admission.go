package readywork

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fanout"
	"github.com/gastownhall/gascity/internal/inputadmission"
)

// rowGate is one read's gate on ready rows: the cross-store blocker rule on a
// split city (cs; nil elsewhere) and native input admission on every city
// (judge, internal/inputadmission). A row either holds back is not ready.
type rowGate struct {
	legs  []Leg
	cs    *crossStore
	judge *inputadmission.Judge
}

// newRowGate returns the gate for one read over legs, judging deferral at now.
func newRowGate(legs []Leg, cs *crossStore, now time.Time) *rowGate {
	return &rowGate{
		legs:  legs,
		cs:    cs,
		judge: inputadmission.New(admissionSources(legs), now),
	}
}

// legIndex returns the index of the leg serving store, or -1.
func (g *rowGate) legIndex(store beads.StoreReader) int {
	for i, leg := range g.legs {
		if beads.StoreReader(leg.Store) == store {
			return i
		}
	}
	return -1
}

// admissionSources are legs as the stores input admission reads, each named by
// its leg's label.
func admissionSources(legs []Leg) []inputadmission.Source {
	sources := make([]inputadmission.Source, len(legs))
	for i, leg := range legs {
		sources[i] = inputadmission.Source{Label: leg.Label}
		if leg.Store != nil {
			sources[i].Reader = leg.Store
		}
	}
	return sources
}

// heldRows returns which of rows, every one served by legs[own], the gate
// holds back: a cross-store blocker holds it, or its workflow's input
// withholds it. Every verdict is memoized for the gate's life.
func (g *rowGate) heldRows(ctx context.Context, own int, rows []beads.Bead) (map[string]bool, error) {
	held, err := g.cs.blockedRows(ctx, own, rows)
	if err != nil {
		return nil, err
	}
	withheld, err := g.judge.Judge(ctx, own, rows)
	if err != nil {
		return nil, err
	}
	for id := range withheld {
		held[id] = true
	}
	return held, nil
}

// gate returns the rows of items the gate does not hold back, in their order,
// judging only as many as the bound needs: rows are judged a chunk at a time,
// grouped by the leg that served them, until limit rows are kept or items run
// out.
func (g *rowGate) gate(ctx context.Context, owners map[string]int, items []beads.Bead, limit int) ([]beads.Bead, error) {
	chunk := len(items)
	if limit > 0 {
		chunk = limit + boundedSlack
	}
	var kept []beads.Bead
	for start := 0; start < len(items) && (limit <= 0 || len(kept) < limit); start += chunk {
		part := items[start:min(start+chunk, len(items))]
		byLeg := make(map[int][]beads.Bead)
		var order []int
		for _, row := range part {
			idx, ok := owners[row.ID]
			if !ok {
				continue
			}
			if _, seen := byLeg[idx]; !seen {
				order = append(order, idx)
			}
			byLeg[idx] = append(byLeg[idx], row)
		}
		held := make(map[string]bool, len(part))
		errs := make([]error, len(order))
		results := make([]map[string]bool, len(order))
		fanout.Bounded(len(order), LegFanOut, func(i int) {
			results[i], errs[i] = g.heldRows(ctx, order[i], byLeg[order[i]])
		})
		for i, err := range errs {
			if err != nil {
				return nil, err
			}
			for id, h := range results[i] {
				held[id] = h
			}
		}
		for _, row := range part {
			if !held[row.ID] {
				kept = append(kept, row)
			}
		}
	}
	if limit > 0 && len(kept) > limit {
		kept = kept[:limit]
	}
	return kept, nil
}

// InputJudge judges rows a reader holds outside a gated read — a controller
// probe's live read of one seat's work — for input admission over legs, the
// stores the city's ready reader federates (#8832): the rule `gc ready` and
// the demand gate apply, over the same stores. A probe that judged over the
// store it read the row from alone would miss a split city's relation (the
// root and its steps in the binding, the input convoy and its members in the
// work store) and count as work a step the hook refuses.
//
// An InputJudge memoizes every root, convoy and stopped-workflow verdict it
// reads for its life: build one per controller pass and never keep it across
// passes. It is safe for concurrent use.
type InputJudge struct {
	legs  []Leg
	judge *inputadmission.Judge
}

// NewInputJudge returns a judge over legs, judging deferral at now. Nil legs
// (a topology that could not be planned) judge nothing.
func NewInputJudge(legs []Leg, now time.Time) *InputJudge {
	return &InputJudge{legs: legs, judge: inputadmission.New(admissionSources(legs), now)}
}

// Withheld returns which of rows, read from store, input admission withholds,
// keyed by row id with each verdict. A workflow root is looked up first in the
// leg over the same store (beads.SameStore, which sees through class handles);
// a store that is none of the legs, or nil, is read through the legs alone.
// A read that fails fails the call, naming the bead and the store, and a nil
// judge withholds nothing.
func (j *InputJudge) Withheld(ctx context.Context, store beads.StoreReader, rows []beads.Bead) (map[string]inputadmission.Withhold, error) {
	if j == nil || len(rows) == 0 {
		return nil, nil
	}
	own := -1
	for i, leg := range j.legs {
		if leg.Store != nil && store != nil && beads.SameStore(leg.Store, store) {
			own = i
			break
		}
	}
	return j.judge.Judge(ctx, own, rows)
}

// JudgeInput judges rows, which legs[own] holds, for input admission with the
// gate's own judge, so a reader that needs verdicts on rows beyond the ready
// ones it gated (the controller's assigned open rows) reads each root, convoy
// and stopped workflow once for the gate's life. It returns the withheld rows
// keyed by id with each verdict; a failed read fails the call, naming the bead
// and the store. An own outside the legs withholds nothing.
func (g *LegGate) JudgeInput(ctx context.Context, own int, rows []beads.Bead) (map[string]inputadmission.Withhold, error) {
	if g == nil || own < 0 || own >= len(g.legs) || len(rows) == 0 {
		return nil, nil
	}
	return g.rows.judge.Judge(ctx, own, rows)
}
