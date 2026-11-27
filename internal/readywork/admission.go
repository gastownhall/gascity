package readywork

import (
	"context"
	"maps"
	"sync"
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

	// withheld records every row input admission withheld over the gate's
	// life, with its verdict, for the demand pass that reports them
	// (LegGate.Withheld).
	mu       sync.Mutex
	withheld map[string]inputadmission.Withhold
}

// newRowGate returns the gate for one read over legs, judging deferral at now.
func newRowGate(legs []Leg, cs *crossStore, now time.Time) *rowGate {
	return &rowGate{
		legs:     legs,
		cs:       cs,
		judge:    inputadmission.New(admissionSources(legs), now),
		withheld: make(map[string]inputadmission.Withhold),
	}
}

// withheldRows returns a copy of every row input admission has withheld over
// the gate's life, keyed by row id.
func (g *rowGate) withheldRows() map[string]inputadmission.Withhold {
	g.mu.Lock()
	defer g.mu.Unlock()
	return maps.Clone(g.withheld)
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
	g.mu.Lock()
	for id, w := range withheld {
		held[id] = true
		g.withheld[id] = w
	}
	g.mu.Unlock()
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

// InputWithheld judges rows, one leg's assigned rows read live by a controller
// probe, for input admission over that leg's store alone, and returns the
// withheld row ids (#8832). The hook's claim path refuses a withheld row, so a
// wake, keep-awake, park or ladder probe that counts it as work holds a session
// for work it cannot claim. The verdict is the rule the ready reader applies,
// read live.
//
// A leg with a nil store, no rows, or a judgment that fails withholds nothing:
// the probe keeps its ungated answer rather than failing a seat decision from a
// read hiccup, the never-under-count rule the demand pass's gate applies
// (PartialResultError there, a conservative answer here).
func InputWithheld(leg Leg, rows []beads.Bead) map[string]bool {
	if leg.Store == nil || len(rows) == 0 {
		return nil
	}
	label := leg.Label
	if label == "" {
		label = "assigned"
	}
	judge := inputadmission.New([]inputadmission.Source{{Label: label, Reader: leg.Store}}, time.Now())
	verdicts, err := judge.Judge(context.Background(), 0, rows)
	if err != nil || len(verdicts) == 0 {
		return nil
	}
	withheld := make(map[string]bool, len(verdicts))
	for id := range verdicts {
		withheld[id] = true
	}
	return withheld
}
