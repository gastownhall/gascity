package main

import (
	"fmt"
	"log"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// The wisp GC's leg readers (typed-stores T7g; t7g-design.md §4, owner
// decision 3). One pass plans the epoch's Molecule{Roots} federation: the
// graph placement, then the work legs the census names (wispGCLegs, every
// leg but the graph store), by ref. The census decides WHICH legs: they are
// the legs every seat-work question reads, and only when the routes relocate
// graph, which is the switch the GC has always used. The plan decides HOW
// each is read: through a federated leg, whose misses another leg covers.
// Writes (closes, purges) stay on the census's own stores.
//
// A leg the plan resolves to a store other than the census's for its ref is
// not read: the census store is what the GC writes to, so reading another
// would decide a purge from rows it is not about to delete. Every such
// mismatch fails closed: the graph arm lists nothing and purges nothing, a
// holder question answers "held", and the work arm skips the leg.
//
// A worker site whose descriptor attaches none of the binding's classes is
// such a mismatch: its census still lists the raw binding as a leg (it is not
// the graph store the epoch hands out, which refuses), while the epoch names
// the binding's ref over the refusing store (T7c). The pre-T7g GC read the
// raw binding there; now that leg fails closed
// (TestWispGCFailsClosedOnAWorkerSite).

// wispGCRoots is one pass's readers.
type wispGCRoots struct {
	// graph reads the graph placement (the Authority leg); zero when the
	// plan's graph leg is not the pass's graph store.
	graph    beads.FederatedReader
	graphErr error
	// work reads each census work leg, by its ref.
	work map[string]beads.FederatedReader
	// workErr is why a census ref has no reader: the plan failed, or the
	// leg resolved to another store.
	workErr map[string]error
	// planErr is why no work leg has a reader.
	planErr error
}

// graphReader is the graph placement's reader, or why there is none.
func (r wispGCRoots) graphReader() (beads.FederatedReader, error) {
	if beads.IsNilStore(r.graph) {
		if r.graphErr != nil {
			return beads.FederatedReader{}, r.graphErr
		}
		return beads.FederatedReader{}, fmt.Errorf("the wisp GC pass has no reader for the graph store")
	}
	return r.graph, nil
}

// workLeg is the reader of the census work leg ref names, or why there is
// none.
func (r wispGCRoots) workLeg(ref string) (beads.FederatedReader, error) {
	if leg, ok := r.work[ref]; ok {
		return leg, nil
	}
	if err := r.workErr[ref]; err != nil {
		return beads.FederatedReader{}, err
	}
	if r.planErr != nil {
		return beads.FederatedReader{}, r.planErr
	}
	return beads.FederatedReader{}, fmt.Errorf("the wisp GC pass has no reader for work leg %q", ref)
}

// wispGCGraphRoots is the pass's readers when the GC reads no work leg (a
// city whose routes relocate nothing): stores' Molecule{Roots} plan with no
// census refs, the graph placement alone. graph is the graph store the pass
// holds.
func wispGCGraphRoots(stores storeref.Stores, graph beads.StoreReader) wispGCRoots {
	return planWispGCRoots(stores, graph, nil, nil)
}

// planWispGCRoots plans stores' Molecule{Roots} over refs and checks each leg
// against the store the pass holds for it: graph for the graph placement,
// census[ref] for a work leg (see the file comment).
func planWispGCRoots(stores storeref.Stores, graph beads.StoreReader, refs []storeref.StoreRef, census map[string]beads.StoreReader) wispGCRoots {
	out := wispGCRoots{work: map[string]beads.FederatedReader{}, workErr: map[string]error{}}
	plan, err := stores.Plan(storeref.Molecule{Kind: storeref.MoleculeRoots, Scope: storeref.WorkRef, Work: refs})
	if err != nil && len(refs) > 0 {
		// The work legs cannot be planned: the graph arm still reads the
		// graph placement, and no work leg is read.
		out.planErr = fmt.Errorf("planning the wisp GC's work legs: %w", err)
		plan, err = stores.Plan(storeref.Molecule{Kind: storeref.MoleculeRoots, Scope: storeref.WorkRef})
	}
	if err != nil {
		out.graphErr = fmt.Errorf("planning the wisp GC's graph leg: %w", err)
		return out
	}
	storeref.EachTypedLeg(plan, func(l storeref.TypedLeg) {
		if l.Role == storeref.RoleAuthority {
			if !beads.SameStore(l.Reader, graph) {
				out.graphErr = fmt.Errorf("the wisp GC's graph leg %q is not the graph store the pass holds", l.Ref)
				return
			}
			out.graph = l.Reader
			return
		}
		ref := string(l.Ref)
		if want, ok := census[ref]; !ok || !beads.SameStore(l.Reader, want) {
			out.workErr[ref] = fmt.Errorf("the wisp GC's work leg %q is not the census's store for it", ref)
			return
		}
		out.work[ref] = l.Reader
	})
	return out
}

// holdsLiveAttachmentSource is storeHoldsLiveAttachmentSource over a pass
// reader that may be missing: no reader is no proof, so the root is held.
func holdsLiveAttachmentSource(leg beads.FederatedReader, err error, rootID string) bool {
	if err != nil {
		log.Printf("wisp gc: cannot read a leg for holders of stepless root %s (%v); leaving it open", rootID, err)
		return true
	}
	return storeHoldsLiveAttachmentSource(leg, rootID)
}
