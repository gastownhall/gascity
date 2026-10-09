package main

import (
	"maps"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// The row-write effect (CONTRACT v5 R2, D-9): an admitted row write commits
// only what decideRow decides again, with the pass's World and allocation,
// on the row the transaction (runTx) reads from the backing, and only when
// that decides the admitted kind.

// Row-write refusal causes. A refusal backs the row off (P4).
const (
	causeRedecided = "redecided" // the fresh row decides another kind, or nothing
	causeCAS       = "cas"       // another writer landed between the read and the write
	causeNoWriter  = "no-conditional-writer"
	causeWrite     = "write-error"
)

// rowWriteSections are the row write's one section.
var rowWriteSections = []section{{Decide: redecideRow}}

// redecideRow is the row write's Decide: decideRow on the fresh row.
func redecideRow(v txView) txStep {
	w := v.World.withRow(v.It.Key, v.Row, v.Meta)
	fresh, _ := decideRow(&w, v.Alloc, v.It.Key)
	if fresh.Kind != v.It.Kind || len(fresh.Patch) == 0 {
		return txStep{Refuse: causeRedecided}
	}
	step := txStep{Write: fresh.Patch}
	if fresh.Event != nil {
		step.Facts.Events = []events.Event{*fresh.Event}
	}
	return step
}

// withRow is w with k's census row read again as row, with its persisted
// metadata meta: removed when the row closed, otherwise rebuilt from row on
// its leg.
func (w World) withRow(k rowKey, row session.Info, meta map[string]string) World {
	c := *w.Census
	c.Rows = maps.Clone(c.Rows)
	if row.Closed {
		delete(c.Rows, k)
	} else {
		c.Rows[k] = w.Census.reread(k, row, meta)
	}
	w.Census = &c
	return w
}
