package orders

import "github.com/gastownhall/gascity/internal/beads"

// The order reads over read-only legs (typed-stores T7b.2): the same reads as
// the Store methods they are named for, taken over a storeref plan leg rather
// than an order front door, so a federated reader (the API's order history
// and feed; T7f's dispatcher and `gc order check`) reads each leg in the
// role its plan declares and mints no orders handle over it. Each shares its
// query and decode with its method, so the two cannot drift; a zero leg reads
// nothing, as a zero OrdersStore does.

// ListTrackingOn is ListTracking over one leg: every tracking bead, both
// tiers, newest first, decoded; rows and any list error are returned
// together.
func ListTrackingOn(leg beads.FederatedReader, limit int) ([]OrderRun, error) {
	if beads.IsNilStore(leg) {
		return nil, nil
	}
	list, err := leg.List(trackingListQuery(limit))
	return trackingRuns(list), err
}

// LatestOpenRunOn is LatestOpenRun over one leg: the newest OPEN run of
// scoped, found, and any list error (found may be true beside a partial-tier
// error).
func LatestOpenRunOn(leg beads.FederatedReader, scoped string) (OrderRun, bool, error) {
	if beads.IsNilStore(leg) {
		return OrderRun{}, false, nil
	}
	list, err := leg.List(latestOpenRunQuery(scoped))
	return latestOpenRun(scoped, list, err)
}

// CursorAcrossLegs is CursorAcross over read-only legs: each leg is read once,
// LIVE (beads.LiveFor, in the leg's role), for the named order's newest run
// records, as Store.Cursor reads each of its stores; the max seq wins, and a
// failed leg is logged and contributes its partial rows, or nothing. A zero
// leg is skipped. Its name keeps the bd trace attribution CursorAcross has
// (internal/beads/bdtrace.go matches "orders.CursorAcross").
func CursorAcrossLegs(legs []beads.FederatedReader) CursorFunc {
	return func(name string) uint64 {
		label := labelOrderRunPrefix + name
		var latest uint64
		for _, leg := range legs {
			if beads.IsNilStore(leg) {
				continue
			}
			results, err := NewestRunRecords(func(limit int) ([]beads.Bead, error) {
				return beads.LiveFor(leg).List(runRecordQuery(label, limit, cursorRunRecords))
			}, cursorRunRecords)
			if seq := cursorSeq(name, results, err); seq > latest {
				latest = seq
			}
		}
		return latest
	}
}
