package main

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"time"

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
	// causeSuperseded: the row's lifecycle changed since the pass read it.
	causeSuperseded = "superseded"
)

// rowWrite is one admitted row write.
type rowWrite struct {
	pass *effectPass
	it   intent
	// decide is decideRow; a test supplies its own arm table.
	decide func(*World, *allocDecision, rowKey) (intent, time.Time)
	// sameLifecycle also refuses, with cause superseded, a fresh row whose
	// lifecycle facts differ from the pass's row: legacy's heal fence
	// (ApplyPatchIfLifecycleUnchanged), which sees a wake request too.
	sameLifecycle bool
}

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

// runLocked is run without taking the row's session mutation lock: an
// effect that holds it already, to make a fresh read and this write in one
// section, calls it (the lock is not reentrant).
func (e rowWrite) runLocked(ctx context.Context) settlement {
	writer, ok := e.pass.Writers[e.it.Key.Leg]
	if !ok {
		return settlement{Outcome: settledRefused, Cause: causeNoWriter, Err: errNoConditionalWriter}
	}
	if err := ctx.Err(); err != nil {
		return settlement{Outcome: settledFailed, Cause: causeDeadline, Err: err}
	}
	var fresh intent
	decided, superseded := false, false
	pass := e.pass.World.Census.Rows[e.it.Key].Info
	wrote, err := writer.updateMetadataFenced(e.it.Key.ID, 1, func(row session.Info, resp session.PersistedResponse) session.MetadataPatch {
		if e.sameLifecycle && !reflect.DeepEqual(session.LifecycleInputFromInfo(pass), session.LifecycleInputFromInfo(row)) {
			superseded = true
			return nil
		}
		w := e.pass.World.withRow(e.it.Key, row, resp.Metadata)
		fresh, _ = e.decide(&w, e.pass.Alloc, e.it.Key)
		decided = fresh.Kind == e.it.Kind && fresh.Basis == e.it.Basis && len(fresh.Patch) > 0
		if !decided || ctx.Err() != nil { // the last check before the CAS
			return nil
		}
		return fresh.Patch
	})
	switch {
	case errors.Is(err, errNoConditionalWriter):
		return settlement{Outcome: settledRefused, Cause: causeNoWriter, Err: err}
	case err != nil:
		return settlement{Outcome: settledFailed, Cause: causeWrite, Err: err}
	case wrote: // a landing keeps its event, even past the deadline
		s := settlement{Outcome: settledLanded}
		if fresh.Event != nil {
			s.Facts.Events = []events.Event{*fresh.Event}
		}
		return s
	case ctx.Err() != nil:
		return settlement{Outcome: settledFailed, Cause: causeDeadline, Err: ctx.Err()}
	case superseded:
		return settlement{Outcome: settledRefused, Cause: causeSuperseded}
	case decided:
		return settlement{Outcome: settledRefused, Cause: causeCAS}
	}
	return settlement{Outcome: settledRefused, Cause: causeRedecided}
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
