package readywork

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fanout"
	"github.com/gastownhall/gascity/internal/inputadmission"
)

// Native input admission (#8832): the ready arm withholds a graph.v2 step
// whose workflow input is not currently admissible. The rule itself lives in
// internal/inputadmission, read live over the same legs this reader
// federates; this file is the glue that applies it to the ready arm's answer
// and to the bounded leg reads that feed it.
//
// The judge is built once per Read (and once per LegGate) and shared by the
// bounded read's refill loop and the final gate, so every root, convoy and
// prerequisite it resolves is read once and every consumer of the same read
// agrees.

// newAdmission returns the input-admission judge over legs' stores, labeled
// by leg. The judge's source indexes are the leg indexes, so a leg index is a
// source index: a leg with a nil store is a source that contributes nothing.
// It returns nil when no leg names a store, a city with nothing to read.
//
// The judge takes typed handles, so this is where each leg's raw store becomes
// its work-class view — the reader's leg boundary is where stores were
// resolved, the one place a handle can be minted on the way in.
func newAdmission(legs []Leg) *inputadmission.Judge {
	sources := make([]inputadmission.Source, len(legs))
	named := false
	for i, leg := range legs {
		if leg.Store != nil {
			sources[i] = inputadmission.Source{
				Label: leg.Label,
				Live:  beads.LiveFor(leg.Store),
				Work:  beads.WorkStore{Store: leg.Store},
			}
			named = true
		}
	}
	if !named {
		return nil
	}
	return inputadmission.New(sources, time.Now())
}

// legIndexFor returns the index of the leg whose store has key — the leg the
// bounded read is serving — or -1.
func legIndexFor(legs []Leg, key string) int {
	for i, leg := range legs {
		if leg.Store != nil && beads.PhysicalStoreKey(leg.Store) == key {
			return i
		}
	}
	return -1
}

// gateItems is the ready arm's gate on every city: rows a cross-store blocker
// holds are dropped when the city is split (crossstore.go), and rows input
// admission withholds are dropped always, judged a chunk at a time until
// limit rows are kept or the items run out. Every verdict is memoized in the
// judge, so a row the bounded read already judged is not read again.
func gateItems(ctx context.Context, cs *crossStore, adm *inputadmission.Judge, owners map[string]int, items []beads.Bead, limit int) ([]beads.Bead, error) {
	if len(items) == 0 {
		return items, nil
	}
	chunk := len(items)
	if limit > 0 {
		chunk = limit + boundedSlack
	}
	var kept []beads.Bead
	for start := 0; start < len(items) && (limit <= 0 || len(kept) < limit); start += chunk {
		part := items[start:min(start+chunk, len(items))]
		if cs != nil {
			admissible, err := cs.gateChunk(ctx, owners, part)
			if err != nil {
				return nil, err
			}
			part = admissible
		}
		admissible, err := dropWithheld(ctx, adm, owners, part)
		if err != nil {
			return nil, err
		}
		kept = append(kept, admissible...)
	}
	if limit > 0 && len(kept) > limit {
		kept = kept[:limit]
	}
	return kept, nil
}

// dropWithheld drops the rows input admission withholds, grouped by the leg
// that served them so the judge reads each row's root from its own store
// first. A nil judge (no legs name a store) keeps every row.
func dropWithheld(ctx context.Context, adm *inputadmission.Judge, owners map[string]int, part []beads.Bead) ([]beads.Bead, error) {
	if adm == nil || len(part) == 0 {
		return part, nil
	}
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
	withheld := make(map[string]struct{}, len(part))
	errs := make([]error, len(order))
	results := make([]map[string]struct{}, len(order))
	fanout.Bounded(len(order), LegFanOut, func(i int) {
		dropped, err := withheldRows(ctx, adm, order[i], byLeg[order[i]])
		if err != nil {
			errs[i] = err
			return
		}
		results[i] = dropped
	})
	for i, err := range errs {
		if err != nil {
			return nil, err
		}
		for id := range results[i] {
			withheld[id] = struct{}{}
		}
	}
	out := make([]beads.Bead, 0, len(part))
	for _, row := range part {
		if _, held := withheld[row.ID]; held {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

// withheldRows judges rows — every one served by source own — and returns
// which input admission withholds. It is the bounded read's half of the refill
// rule: a withheld row is not a row in hand, so it does not count toward the
// limit.
func withheldRows(ctx context.Context, adm *inputadmission.Judge, own int, rows []beads.Bead) (map[string]struct{}, error) {
	if adm == nil || len(rows) == 0 {
		return nil, nil
	}
	verdicts, err := adm.Judge(ctx, own, rows)
	if err != nil {
		return nil, err
	}
	withheld := make(map[string]struct{}, len(verdicts))
	for id := range verdicts {
		withheld[id] = struct{}{}
	}
	return withheld, nil
}
