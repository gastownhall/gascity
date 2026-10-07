package main

import (
	"context"
	"errors"
	"maps"
	"time"

	"github.com/gastownhall/gascity/internal/session"
)

// The row-write effect (CONTRACT v5 R2, D-9): an admitted row write commits
// only what decideRow decides again on the fresh row. Under the process-wide
// row lock, with its context checked inside the lock (an effect abandoned at
// its deadline never writes), it re-reads the row through the cache, re-runs
// decideRow on that read with the pass's World and allocation, and writes the
// re-decided intent's patch with a CAS at that read's revision, but only when
// the re-decided intent has the admitted kind and basis (the incarnation the
// pass saw, as legacy's authorized checks). It writes through the fenced
// writer of the row's own census leg. A lost CAS writes nothing: the
// store evicts the row, and the next pass decides again.

// Row-write refusal causes. A refusal backs the row off (P4).
const (
	causeRedecided = "redecided" // the fresh row decides another kind, or nothing
	causeCAS       = "cas"       // another writer landed between the read and the write
	causeNoWriter  = "no-conditional-writer"
	causeWrite     = "write-error"
)

// rowWrite is one admitted row write.
type rowWrite struct {
	pass *effectPass
	it   intent
	// decide is decideRow; a test supplies its own arm table.
	decide func(*World, *allocDecision, rowKey) (intent, time.Time)
}

func rowWriteEffect(p *effectPass, it intent) func(context.Context) settlement {
	return rowWrite{pass: p, it: it, decide: decideRow}.run
}

func (e rowWrite) run(ctx context.Context) settlement {
	writer, ok := e.pass.Writers[e.it.Key.Leg]
	if !ok {
		return settlement{Outcome: settledRefused, Cause: causeNoWriter, Err: errNoConditionalWriter}
	}
	var fresh intent
	decided := false
	var wrote bool
	err := session.WithSessionMutationLock(e.it.Key.ID, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		wrote, err = writer.updateMetadataFenced(e.it.Key.ID, 1, func(row session.Info, _ session.PersistedResponse) session.MetadataPatch {
			w := e.pass.World.withRow(e.it.Key, row)
			fresh, _ = e.decide(&w, e.pass.Alloc, e.it.Key)
			decided = fresh.Kind == e.it.Kind && fresh.Basis == e.it.Basis && len(fresh.Patch) > 0
			if !decided || ctx.Err() != nil { // the last check before the CAS
				return nil
			}
			return fresh.Patch
		})
		return err
	})
	switch {
	case errors.Is(err, errNoConditionalWriter):
		return settlement{Outcome: settledRefused, Cause: causeNoWriter, Err: err}
	case err != nil:
		return settlement{Outcome: settledFailed, Cause: causeWrite, Err: err}
	case wrote: // a landing keeps its event, even past the deadline
		return settlement{Outcome: settledLanded, Event: fresh.Event}
	case ctx.Err() != nil:
		return settlement{Outcome: settledFailed, Cause: causeDeadline, Err: ctx.Err()}
	case decided:
		return settlement{Outcome: settledRefused, Cause: causeCAS}
	}
	return settlement{Outcome: settledRefused, Cause: causeRedecided}
}

// withRow is w with k's census row read again as row: removed when the row
// closed, otherwise rebuilt from row on its leg.
func (w World) withRow(k rowKey, row session.Info) World {
	c := *w.Census
	c.Rows = maps.Clone(c.Rows)
	if row.Closed {
		delete(c.Rows, k)
	} else {
		r := newCensusRow(k, row)
		if r.DuplicateOf = c.Rows[k].DuplicateOf; r.DuplicateOf != "" {
			r.PendingCreate = false
		}
		c.Rows[k] = r
	}
	w.Census = &c
	return w
}
