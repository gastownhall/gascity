package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// announceBdPassthroughCloses records bead.closed for each bead a successful
// `gc bd close` (or `gc bd update --status closed`) passthrough just closed.
//
// The passthrough hands the close to a bd subprocess against the work store,
// the way the stock formulas tell a worker to finish its step, and that write
// announces nothing. The supervisor's run views fold only the city event log,
// so the step stayed active there until the supervisor read the row back. This
// is the close half of hook_claim_bead_events.go and uses the same one-shot
// emitter: the canonical post-close snapshot with its edges confirmed, the
// run correlation on the envelope, appended under the log's cross-process lock.
//
// Only a real open-to-closed transition is announced. The supervisor runs a
// committed close's completion and autoclose effects for every bead.closed
// from a writer that is not its cache, so a bead that was already closed
// before the passthrough (per the exact-ID guard's pre-read) gets nothing, and
// neither does one whose read-back does not show it closed. A read-back that
// fails skips the row rather than recording a bare id, which would clobber the
// step's title and run membership in the fold.
//
// A bulk close (the maintenance orders close stale wisps in batches) reads
// its rows back in one batched read and confirms their edges in one more,
// rather than one or two bd forks per id, for the reason the exact-ID guard
// batches its pre-read.
//
// Best-effort: the close has committed. Failures are reported once on stderr
// (warnClassStoreEmit) and the supervisor's read-back still catches the close
// up as before. A close of a bead in a relocated class never reaches here:
// maybeRouteBdByID serves it in process through a store that announces its own
// writes. A `bd close` the agent runs directly, without gc, is not seen by gc
// at all and still waits for the supervisor's read-back.
func announceBdPassthroughCloses(cityPath string, store beads.Store, before map[string]beads.Bead, bdArgs []string) {
	cityPath = strings.TrimSpace(cityPath)
	if cityPath == "" || store == nil {
		return
	}
	ids, ok := workRecordCloseTargets(bdArgs)
	if !ok {
		return
	}
	targets := make([]string, 0, len(ids))
	for _, id := range ids {
		if prior, read := before[id]; read && beadStatusIsClosed(prior.Status) {
			continue
		}
		targets = append(targets, id)
	}
	if len(targets) == 0 {
		return
	}
	emitter := &emittingClassStore{Store: store, cityPath: cityPath}
	closed := closedSnapshots(emitter, targets, before)
	emissions := make([]classStoreEmission, 0, len(closed))
	for _, bead := range closed {
		emissions = append(emissions, classStoreEmission{eventType: events.BeadClosed, bead: bead})
	}
	emitter.emit(emissions...)
}

// closedSnapshots reads ids back after a close and returns, in input order,
// the ones that now read closed, each with its edge set confirmed.
//
// The edges of an id whose pre-close row (before) carried none are read at the
// same time as the rows, not after them. A close does not change a bead's
// edges, so that read does not wait on the read-back, and on a bd-backed store
// each read is its own bd subprocess: read one after the other, they put two
// serial round trips after every close of an edgeless bead, on top of the
// guard's read and the close itself (ga-b5kq9js).
func closedSnapshots(emitter *emittingClassStore, ids []string, before map[string]beads.Bead) []beads.Bead {
	var edgeless []string
	for _, id := range ids {
		if len(before[id].Dependencies) == 0 {
			edgeless = append(edgeless, id)
		}
	}
	edgesRead := make(chan downEdgesRead, 1)
	go func() { edgesRead <- emitter.readDownEdges(edgeless) }()
	read := readBackRows(emitter, ids, events.BeadClosed)
	edges := <-edgesRead
	var closed []beads.Bead
	for _, id := range ids {
		bead, ok := read[id]
		if !ok || !beadStatusIsClosed(bead.Status) {
			continue
		}
		switch {
		case len(bead.Dependencies) > 0:
			bead.Needs = nil
		case edges.answered[id]:
			bead.Dependencies = edges.deps[id]
			if bead.Dependencies == nil {
				bead.Dependencies = []beads.Dep{}
			}
			bead.Needs = nil
		default:
			bead = emitter.withConfirmedEdges(bead)
		}
		closed = append(closed, bead)
	}
	return closed
}

// downEdgesRead is the DOWN edges of a set of beads. answered names the beads
// the read covered, with or without edges; a bead it does not name was not
// read, and its edges are still unknown.
type downEdgesRead struct {
	deps     map[string][]beads.Dep
	answered map[string]bool
}

// readDownEdges reads the DOWN edges of ids: in one batched read where the store
// has one, and with DepList for a single id. A failed read answers for no bead,
// so each is confirmed on its own afterwards.
func (s *emittingClassStore) readDownEdges(ids []string) downEdgesRead {
	var deps map[string][]beads.Dep
	switch len(ids) {
	case 0:
		return downEdgesRead{}
	case 1:
		got, err := s.DepList(ids[0], "down")
		if err != nil {
			return downEdgesRead{}
		}
		deps = map[string][]beads.Dep{ids[0]: got}
	default:
		batch, ok := batchDownEdges(s.Store, ids)
		if !ok {
			return downEdgesRead{}
		}
		deps = batch
	}
	answered := make(map[string]bool, len(ids))
	for _, id := range ids {
		answered[id] = true
	}
	return downEdgesRead{deps: deps, answered: answered}
}

// readBackRows reads ids back after a write: in one batched read where the
// store has one, and one at a time for whatever the batch did not answer
// exactly. An id that cannot be read is left out and reported, never filled
// with a bare id, which would clobber the row in every fold that applies it.
func readBackRows(emitter *emittingClassStore, ids []string, eventType string) map[string]beads.Bead {
	if len(ids) == 0 {
		return nil
	}
	read, err := beads.PrefetchExact(emitter.Store, ids)
	if err != nil || read == nil {
		read = map[string]beads.Bead{}
	}
	for _, id := range ids {
		if _, ok := read[id]; ok {
			continue
		}
		got, err := emitter.Get(id)
		if err != nil || strings.TrimSpace(got.ID) == "" {
			warnClassStoreEmit(fmt.Errorf("%s skipped: %s could not be re-read after the write", eventType, id))
			continue
		}
		read[id] = got
	}
	return read
}

// withConfirmedEdgesBatch is emittingClassStore.withConfirmedEdges for several
// rows at once: the rows that carry no edges have them confirmed in one batched
// read where the store has one, and one at a time otherwise.
func withConfirmedEdgesBatch(emitter *emittingClassStore, rows []beads.Bead) []beads.Bead {
	var edgeless []string
	for _, bead := range rows {
		if len(bead.Dependencies) == 0 {
			edgeless = append(edgeless, bead.ID)
		}
	}
	edges, batched := batchDownEdges(emitter.Store, edgeless)
	out := make([]beads.Bead, 0, len(rows))
	for _, bead := range rows {
		switch {
		case len(bead.Dependencies) > 0:
			bead.Needs = nil
		case batched:
			bead.Dependencies = edges[bead.ID]
			if bead.Dependencies == nil {
				bead.Dependencies = []beads.Dep{}
			}
			bead.Needs = nil
		default:
			bead = emitter.withConfirmedEdges(bead)
		}
		out = append(out, bead)
	}
	return out
}

// batchDownEdges reads the DOWN edges of several beads in one round trip, and
// reports false when it could not (one id, no batch capability, or a failed
// read), leaving the caller to confirm each bead's edges on its own. The beads
// were just read back, so an id the batch answered nothing for has no edges.
func batchDownEdges(store beads.Store, ids []string) (map[string][]beads.Dep, bool) {
	lister, ok := store.(beads.DependencyBatchLister)
	if !ok || len(ids) < 2 {
		return nil, false
	}
	edges, err := lister.DepListBatch(ids)
	if err != nil || edges == nil {
		if err != nil && !errors.Is(err, beads.ErrDepListBatchUnsupported) {
			warnClassStoreEmit(fmt.Errorf("confirming the edges of %d beads: %w", len(ids), err))
		}
		return nil, false
	}
	return edges, true
}
