package convergence

import (
	"strings"
	"time"
)

// ChildStats holds the projections derived from a bead's children, filtered
// to convergence wisps (children whose idempotency key carries the bead's
// convergence prefix). It is computed once from a single Children() fetch so
// the scan sites that used to re-derive these values independently share one
// filter definition.
//
// The per-projection filters intentionally differ, and those differences are
// load-bearing (they predate this consolidation):
//
//   - ClosedCount and CumulativeDur count every closed convergence child,
//     including wisps whose iteration number does not parse.
//   - HighestClosed and HighestOpen additionally require a parseable
//     iteration number, since their whole purpose is to pick the
//     highest-iteration wisp.
type ChildStats struct {
	// ClosedCount is the number of closed convergence wisps.
	ClosedCount int
	// CumulativeDur is the summed lifetime of every closed convergence wisp
	// that has both a non-zero created and closed timestamp.
	CumulativeDur time.Duration

	// HighestClosed is the closed convergence wisp with the highest parseable
	// iteration number. HighestClosedFound is false when none qualifies, and
	// HighestClosedIter is then -1.
	HighestClosed      BeadInfo
	HighestClosedIter  int
	HighestClosedFound bool

	// HighestOpen is the open/in_progress convergence wisp with the highest
	// parseable iteration number. HighestOpenFound is false when none
	// qualifies, and HighestOpenIter is then -1.
	HighestOpen      BeadInfo
	HighestOpenIter  int
	HighestOpenFound bool
}

// childStats derives every convergence child projection from a single
// pre-fetched child list. It is pure: it performs no store I/O, so callers can
// fetch Children() once per transition and read the fields they need.
func childStats(children []BeadInfo, beadID string) ChildStats {
	prefix := IdempotencyKeyPrefix(beadID)
	stats := ChildStats{HighestClosedIter: -1, HighestOpenIter: -1}

	for _, child := range children {
		if !strings.HasPrefix(child.IdempotencyKey, prefix) {
			continue
		}
		iter, iterOK := ParseIterationFromKey(child.IdempotencyKey)

		switch child.Status {
		case "closed":
			stats.ClosedCount++
			if !child.ClosedAt.IsZero() && !child.CreatedAt.IsZero() {
				stats.CumulativeDur += child.ClosedAt.Sub(child.CreatedAt)
			}
			if iterOK && iter > stats.HighestClosedIter {
				stats.HighestClosed = child
				stats.HighestClosedIter = iter
				stats.HighestClosedFound = true
			}
		case "open", "in_progress":
			if iterOK && iter > stats.HighestOpenIter {
				stats.HighestOpen = child
				stats.HighestOpenIter = iter
				stats.HighestOpenFound = true
			}
		}
	}

	return stats
}
