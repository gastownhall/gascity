package legacyop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/opjournal"
)

// SweepStats counts what one Sweep pass did.
type SweepStats struct {
	// Listed is the open legacy ops the pass read.
	Listed int
	// Closed is the expired ops it closed failed{deadline_in_flight}.
	Closed int
	// Pending is the ops still within their finish_by.
	Pending int
	// Raced is the ops someone else closed first (an executor or adopter).
	Raced int
	// Failed is the ops whose close failed; the next pass retries them.
	Failed int
	// Truncated reports that the read stopped at its page cap after a full
	// page: legacy ops behind a head of lane or undecodable rows longer than
	// the cap go unseen, and passes with this limit keep missing them until
	// that head shrinks.
	Truncated bool
	// Undecodable lists the rows the pass could not decode; it never writes
	// them.
	Undecodable []string
}

// Sweep closes open legacy ops whose finish_by has passed as
// failed{deadline_in_flight}, by by, taking at most limit of them (oldest
// first; Store.ListOpenLegacy pages past lane and undecodable rows, up to
// its page cap, and the stats say Truncated when it hit that cap). It never
// touches a lane op nor an op before its finish_by: each close re-decides on
// the row it writes (Store.CloseExpired), so an op an adopter moved after the
// read counts Pending and one a late executor closed first counts Raced. A
// second pass over the same state closes nothing. The error joins the read
// error or every failed close; the stats are valid either way.
func Sweep(st *opjournal.Store, now time.Time, limit int, by string) (SweepStats, error) {
	ops, undecodable, truncated, err := st.ListOpenLegacy(limit)
	stats := SweepStats{Listed: len(ops), Truncated: truncated, Undecodable: undecodable}
	if err != nil {
		return stats, fmt.Errorf("sweeping legacy ops: %w", err)
	}
	var errs []error
	for _, op := range ops {
		if !op.Executor.Legacy() || op.Status == opjournal.StatusClosed {
			continue
		}
		if !now.After(op.FinishBy) {
			stats.Pending++
			continue
		}
		got, closedNow, err := st.CloseExpired(op, now, by)
		switch {
		case err != nil:
			stats.Failed++
			countSweep(op.Kind, ResultFailed)
			errs = append(errs, fmt.Errorf("sweeping op %s: %w", op.ID, err))
		case closedNow:
			stats.Closed++
			countSweep(op.Kind, ResultClosed)
		case got.Status == opjournal.StatusClosed:
			stats.Raced++
			countSweep(op.Kind, ResultLate)
		default:
			stats.Pending++
		}
	}
	return stats, errors.Join(errs...)
}

func countSweep(kind opjournal.Kind, result string) {
	recordOpRecord(context.Background(), string(kind), SurfaceController, StageSweep, result, "")
}
