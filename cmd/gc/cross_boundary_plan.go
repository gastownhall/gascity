package main

// The cross-boundary edge plan both migrations run (S5 §8.1, D9): the rig
// import (rig_import_plan.go, S4b) and the city copy (infra_class_migrate.go).
//
// A migration moves a set of rows out of one store into the city binding.
// Every edge with both ends moving is copied. An edge that would cross the
// boundary afterwards is never written as a cross-store edge; it is encoded
// another way, refused, or dropped and recorded:
//
//   - D -> W (a moved row depends on a row that stays), blocking, D open and
//     W unsatisfied: a `blocks` edge becomes an S3b cross-store gate in the
//     binding (a same-store edge D -> gate the release lane opens once W is
//     satisfied); waits-for and conditional-blocks refuse, the gate encodes
//     only blocks.
//   - W -> D (an open row that stays, blocked by an unsatisfied moved row):
//     refuses. A work store has no gate pattern.
//   - tracks or parent-child with both ends open, either direction: refuses.
//     These are membership edges live code reads (drain-unit convoys).
//   - An "external:" reference a moved row holds is copied as it is, and
//     recorded (copied-external). It names no row in either store, so it
//     crosses no boundary; on a split city the readers enforce it
//     (beads.ExternalBlockerTarget): an "external:<id>" blocker holds its
//     dependent until some store holds <id> and it closes, and a capability
//     reference never blocks. `gc storage preflight` names an open row
//     waiting on one whose bead no store holds.
//   - Every other boundary edge is dropped and recorded on the moved row
//     (gc.migrated_cross_edges).
//
// The plan is a pure function of what the caller read. Nothing here writes,
// so `gc storage preflight` runs exactly the plan a migration runs.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// infraClassMoves is the class decision both migrations plan with: a row
// moves into the binding when its class under gen is not work. Generation
// matters: a verdict judged against a copy manifest selects its rows with the
// manifest's generation (infra_copy_manifest.go).
//
// It is the one place the planner asks which store holds a row's class. At
// T8 it becomes a predicate over storeref.Stores.Holds, so the migration and
// the runtime share one holds rule (typed-stores §5).
func infraClassMoves(gen coordclass.Generation) func(beads.Bead) bool {
	return func(b beads.Bead) bool { return coordclass.ClassifyAt(b, gen) != coordclass.ClassWork }
}

// crossBoundaryInput is everything crossBoundaryPlan reads.
type crossBoundaryInput struct {
	// Snap is the source store: its rows and the down edges of each. A row
	// that stays needs only its edges onto rows that may move.
	Snap rigSnapshot
	// Moves is the class decision (infraClassMoves): a row it accepts is an
	// infrastructure row, whether or not this run moves it.
	Moves func(beads.Bead) bool
	// Moved are the rows this run copies into the binding.
	Moved map[string]bool
	// InBinding are the rows that are, or will be, binding rows: Moved plus
	// what earlier runs carried. Held is what the binding holds under the ids
	// the plan asks about. Both are the rig import's alone: the city copy and
	// its clear move into a binding that holds nothing else (prepare refuses
	// a populated one, and a gate is the copy's own), so for them nothing is
	// in the binding but Moved, and both stay nil.
	InBinding map[string]bool
	Held      map[string]beads.Bead
	// Elsewhere holds every copy, in a store other than the source and the
	// binding, of a blocker an open moved row waits on that neither holds.
	Elsewhere map[string][]beads.Bead
	// ElsewhereUnread names the stores that could not be read for those
	// blockers.
	ElsewhereUnread []string
	// WorkNoun names a row that stays, and SourceNoun the store it stays in,
	// in refusals ("rig work", "the rig").
	WorkNoun, SourceNoun string
}

// crossBoundaryResult is what the plan decided for the edges.
type crossBoundaryResult struct {
	// Copy are the edges written into the binding, both ends there after
	// the run, sorted.
	Copy []beads.Dep
	// Gates are the S3b gates minted for open blocks edges onto work.
	Gates []rigImportGate
	// Records are the boundary edges not copied, keyed by moved row, sorted.
	Records map[string][]rigCrossEdgeRecord
	// Refusals are the shapes no migration may move, each by bead.
	Refusals []rigImportRefusal
}

// crossBoundaryPlan plans every edge touching a moved row.
func crossBoundaryPlan(in crossBoundaryInput) crossBoundaryResult {
	out := crossBoundaryResult{Records: map[string][]rigCrossEdgeRecord{}}
	infra := make(map[string]bool, len(in.Snap.rows))
	for _, b := range in.Snap.rows {
		if in.Moves(b) {
			infra[b.ID] = true
		}
	}
	record := func(id string, rec rigCrossEdgeRecord) {
		out.Records[id] = append(out.Records[id], rec)
	}
	statusOf := func(id string) string {
		if b, ok := in.Snap.byID[id]; ok {
			return b.Status
		}
		if b, ok := in.Held[id]; ok {
			return b.Status
		}
		return ""
	}
	for _, b := range in.Snap.rows {
		for _, d := range in.Snap.down[b.ID] {
			from, to := d.IssueID, d.DependsOnID
			if from == "" {
				from = b.ID
			}
			fromMoved, toMoved := in.Moved[from], in.Moved[to]
			if !fromMoved && !toMoved {
				continue
			}
			edge := beads.Dep{IssueID: from, DependsOnID: to, Type: d.Type}
			switch {
			case fromMoved && toMoved:
				out.Copy = append(out.Copy, edge)
			case fromMoved:
				out.planOutEdge(in, edge, infra, record, statusOf)
			default:
				out.planInEdge(in, edge, infra, record)
			}
		}
	}
	for id := range out.Records {
		recs := out.Records[id]
		sort.Slice(recs, func(i, j int) bool {
			if recs[i].Dir != recs[j].Dir {
				return recs[i].Dir < recs[j].Dir
			}
			return recs[i].Other < recs[j].Other
		})
	}
	sort.Slice(out.Copy, func(i, j int) bool {
		if out.Copy[i].IssueID != out.Copy[j].IssueID {
			return out.Copy[i].IssueID < out.Copy[j].IssueID
		}
		return out.Copy[i].DependsOnID < out.Copy[j].DependsOnID
	})
	return out
}

// planOutEdge decides an edge from a moved row D onto a row that does not
// move.
func (out *crossBoundaryResult) planOutEdge(in crossBoundaryInput, edge beads.Dep, infra map[string]bool, record func(string, rigCrossEdgeRecord), statusOf func(string) string) {
	d := in.Snap.byID[edge.IssueID]
	to := edge.DependsOnID
	if w, ok := in.Snap.byID[to]; ok && !infra[to] {
		out.planWorkBlocker(in, d, w, edge, record)
		return
	}
	if in.InBinding[to] {
		// An infrastructure row an earlier run carried, or one the binding
		// holds as this city's: both ends are binding rows.
		out.Copy = append(out.Copy, edge)
		return
	}
	if strings.HasPrefix(to, "external:") {
		out.Copy = append(out.Copy, edge)
		record(d.ID, rigCrossEdgeRecord{Dir: "out", Type: edge.Type, Other: to, Action: rigEdgeExternal})
		return
	}
	if _, ok := in.Held[to]; ok {
		out.Copy = append(out.Copy, edge)
		return
	}
	out.planElsewhereBlocker(in, d, edge, record, statusOf)
}

// planElsewhereBlocker decides D -> X where neither the source nor the
// binding holds X: X may be held by another store (a rig, the city work
// store). A blocker held elsewhere is encoded as an S3b gate (built for
// exactly a blocker held in another store) or refused; the edge is dropped
// only once X is proven gone from every store.
func (out *crossBoundaryResult) planElsewhereBlocker(in crossBoundaryInput, d beads.Bead, edge beads.Dep, record func(string, rigCrossEdgeRecord), statusOf func(string) string) {
	to := edge.DependsOnID
	rec := rigCrossEdgeRecord{Dir: "out", Type: edge.Type, Other: to, OtherStatus: statusOf(to), Action: rigEdgeDropped}
	blocking := beads.IsReadyBlockingDependencyType(edge.Type)
	if (!blocking && !rigMembershipEdge(edge.Type)) || !rigRowOpen(d) {
		record(d.ID, rec)
		return
	}
	copies := in.Elsewhere[to]
	if len(copies) == 0 {
		if len(in.ElsewhereUnread) > 0 {
			out.Refusals = append(out.Refusals, rigImportRefusal{
				IDs:    []string{d.ID, to},
				Reason: fmt.Sprintf("open %s depends (%s) on %s, which neither %s nor the binding holds, and store(s) that may hold it could not be read (%s)", d.ID, edge.Type, to, in.SourceNoun, strings.Join(in.ElsewhereUnread, "; ")),
				Remedy: "restore access to those stores so the import can find the blocker, then re-run",
			})
			return
		}
		rec.OtherStatus = "absent from every store"
		record(d.ID, rec)
		return
	}
	rec.OtherStatus = copies[0].Status
	if rigMembershipEdge(edge.Type) {
		for _, c := range copies {
			if rigRowOpen(c) {
				out.Refusals = append(out.Refusals, rigImportRefusal{
					IDs:    []string{d.ID, to},
					Reason: fmt.Sprintf("%s and %s (held in another store) are both open and joined by a %s edge, a membership edge that cannot cross stores", d.ID, to, edge.Type),
					Remedy: fmt.Sprintf("close the workflow (or the convoy) %s belongs to, or close %s, then re-run", d.ID, to),
				})
				return
			}
		}
		record(d.ID, rec)
		return
	}
	for _, c := range copies {
		if rigRowSatisfied(c) {
			continue
		}
		if edge.Type != "blocks" {
			out.Refusals = append(out.Refusals, rigImportRefusal{
				IDs:    []string{d.ID, to},
				Reason: fmt.Sprintf("open %s waits (%s) on unfinished %s, held in another store, and a cross-store gate encodes only blocks", d.ID, edge.Type, to),
				Remedy: fmt.Sprintf("finish %s, or close %s's workflow, then re-run", to, d.ID),
			})
			return
		}
		out.Gates = append(out.Gates, rigImportGate{Owner: d, Blocker: to})
		rec.Action = rigEdgeGated
		break
	}
	record(d.ID, rec)
}

// planWorkBlocker decides D -> W where W is work that stays.
func (out *crossBoundaryResult) planWorkBlocker(in crossBoundaryInput, d, w beads.Bead, edge beads.Dep, record func(string, rigCrossEdgeRecord)) {
	rec := rigCrossEdgeRecord{Dir: "out", Type: edge.Type, Other: w.ID, OtherStatus: w.Status, Action: rigEdgeDropped}
	switch {
	case rigMembershipEdge(edge.Type):
		if rigRowOpen(d) && rigRowOpen(w) {
			out.Refusals = append(out.Refusals, rigImportRefusal{
				IDs:    []string{d.ID, w.ID},
				Reason: fmt.Sprintf("%s and %s %s are both open and joined by a %s edge, a membership edge that cannot cross stores", d.ID, in.WorkNoun, w.ID, edge.Type),
				Remedy: fmt.Sprintf("close the workflow (or the convoy) %s belongs to, or close %s, then re-run", d.ID, w.ID),
			})
			return
		}
	case beads.IsReadyBlockingDependencyType(edge.Type):
		if rigRowOpen(d) && !rigRowSatisfied(w) {
			if edge.Type != "blocks" {
				out.Refusals = append(out.Refusals, rigImportRefusal{
					IDs:    []string{d.ID, w.ID},
					Reason: fmt.Sprintf("open %s waits (%s) on unfinished %s %s, and a cross-store gate encodes only blocks", d.ID, edge.Type, in.WorkNoun, w.ID),
					Remedy: fmt.Sprintf("finish %s, or close %s's workflow, then re-run", w.ID, d.ID),
				})
				return
			}
			out.Gates = append(out.Gates, rigImportGate{Owner: d, Blocker: w.ID})
			rec.Action = rigEdgeGated
		}
	}
	record(d.ID, rec)
}

// planInEdge decides an edge from a row that does not move onto a moved row
// D.
func (out *crossBoundaryResult) planInEdge(in crossBoundaryInput, edge beads.Dep, infra map[string]bool, record func(string, rigCrossEdgeRecord)) {
	from := edge.IssueID
	d := in.Snap.byID[edge.DependsOnID]
	if in.InBinding[from] {
		if _, ok := in.Held[from]; ok {
			// An earlier run carried the dependent: both ends are now
			// binding rows.
			out.Copy = append(out.Copy, edge)
			return
		}
	}
	w, ok := in.Snap.byID[from]
	if !ok || infra[from] {
		record(d.ID, rigCrossEdgeRecord{Dir: "in", Type: edge.Type, Other: from, Action: rigEdgeDropped})
		return
	}
	switch {
	case rigMembershipEdge(edge.Type):
		if rigRowOpen(d) && rigRowOpen(w) {
			out.Refusals = append(out.Refusals, rigImportRefusal{
				IDs:    []string{w.ID, d.ID},
				Reason: fmt.Sprintf("%s %s and %s are both open and joined by a %s edge, a membership edge that cannot cross stores", in.WorkNoun, w.ID, d.ID, edge.Type),
				Remedy: fmt.Sprintf("close the workflow (or the convoy) %s belongs to, or close %s, then re-run", d.ID, w.ID),
			})
			return
		}
	case beads.IsReadyBlockingDependencyType(edge.Type):
		if rigRowOpen(w) && !rigRowSatisfied(d) {
			out.Refusals = append(out.Refusals, rigImportRefusal{
				IDs:    []string{w.ID, d.ID},
				Reason: fmt.Sprintf("open %s %s is blocked (%s) by open %s, and a work store cannot hold a cross-store gate", in.WorkNoun, w.ID, edge.Type, d.ID),
				Remedy: fmt.Sprintf("finish or cancel %s's workflow, or remove the edge with `gc bd dep remove %s %s`, then re-run", d.ID, w.ID, d.ID),
			})
			return
		}
	}
	record(d.ID, rigCrossEdgeRecord{Dir: "in", Type: edge.Type, Other: w.ID, OtherStatus: w.Status, Action: rigEdgeDropped})
}
