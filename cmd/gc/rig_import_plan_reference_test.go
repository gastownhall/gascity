package main

// The rig importer's edge planner as it stood before S5-D lifted it into the
// shared crossBoundaryPlan (cross_boundary_plan.go), kept verbatim as the
// oracle the equivalence tests run the lifted planner against
// (cross_boundary_plan_equivalence_test.go). Delete this file once S5-G
// lands: from then on the shared planner is the only one either migration
// has ever shipped with.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// refPlanRigImport is planRigImport before the lift: it plans one rig's import. A row whose scope names another city
// or another rig, or that another city's agent claims, refuses.
func refPlanRigImport(ctx rigPlanContext) rigImportPlan {
	rig, cityName, snap, manifest, held := ctx.Rig, ctx.CityName, ctx.Snap, ctx.Manifest, ctx.Held
	plan := rigImportPlan{Rig: rig, Generation: coordclass.CurrentGeneration, Records: map[string][]rigCrossEdgeRecord{}, RetainedRecords: map[string][]rigCrossEdgeRecord{}}
	if manifest != nil {
		plan.Generation = manifest.Generation
	}
	stamp := rigImportStamp(rig)
	infra := map[string]bool{}
	moved := map[string]bool{}
	inBinding := map[string]bool{} // rows that are (or will be) binding rows
	var collisions, reused, foreign, claimed, unheld, unreadable []string
	for _, b := range snap.rows {
		if coordclass.ClassifyAt(b, plan.Generation) == coordclass.ClassWork {
			continue
		}
		infra[b.ID] = true
		plan.Infra++
		h, holds := held[b.ID]
		if manifest != nil && manifest.Carried(b) {
			switch {
			case !holds:
				// The rig holds the only copy. It is never read as absent
				// (an edge onto it recorded as onto a bead no store holds).
				unheld = append(unheld, b.ID)
				continue
			case h.Metadata[infraMigrationStampKey] != stamp:
				collisions = append(collisions, b.ID)
				continue
			}
			if raw := strings.TrimSpace(h.Metadata[beadmeta.MigratedCrossEdgesMetadataKey]); raw != "" {
				var recs []rigCrossEdgeRecord
				if err := json.Unmarshal([]byte(raw), &recs); err != nil {
					unreadable = append(unreadable, fmt.Sprintf("%s (%v)", b.ID, err))
					continue
				}
				plan.RetainedRecords[b.ID] = recs
			}
			plan.Retained = append(plan.Retained, b)
			inBinding[b.ID] = true
			continue
		}
		if ref := strings.TrimSpace(b.Metadata[beadmeta.RootStoreRefMetadataKey]); rigScopeIsForeign(ref, rig, cityName) {
			foreign = append(foreign, fmt.Sprintf("%s (%s)", b.ID, ref))
			continue
		}
		if a := strings.TrimSpace(b.Assignee); a != "" && rigRowOpen(b) && ctx.ForeignClaimant != nil && ctx.ForeignClaimant(a) {
			claimed = append(claimed, fmt.Sprintf("%s (claimed by %s)", b.ID, a))
			continue
		}
		if holds {
			switch {
			case h.Metadata[infraMigrationStampKey] == stamp && (manifest == nil || !manifest.IDs[b.ID]):
				// An interrupted earlier attempt's copy: prepare deletes it.
			case h.Metadata[infraMigrationStampKey] == stamp:
				// The manifest records this id for a different bead: the
				// rig minted it again after the import.
				reused = append(reused, b.ID)
				continue
			case h.Metadata[infraMigrationStampKey] != "" && beadCopyDifference(b, h) == "":
				plan.Shared = append(plan.Shared, b.ID)
				inBinding[b.ID] = true
				continue
			default:
				collisions = append(collisions, b.ID)
				continue
			}
		}
		plan.Move = append(plan.Move, b)
		moved[b.ID] = true
		inBinding[b.ID] = true
		if rigRowOpen(b) {
			plan.Open++
			if strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) != "" {
				plan.OpenRouted++
			}
		}
		if strings.TrimSpace(b.Metadata[beadmeta.RootStoreRefMetadataKey]) == "" {
			plan.Annotated = append(plan.Annotated, b.ID)
		}
	}
	if len(unheld) > 0 {
		sort.Strings(unheld)
		plan.Refusals = append(plan.Refusals, rigImportRefusal{
			IDs:    unheld,
			Reason: "the rig's manifest records each of these rows as carried into the city binding, but the binding no longer holds it, so the rig row may be the only copy",
			Remedy: "restore the binding database from its backup, or, if the binding removed a row on purpose since the import, remove the rig row too, then re-run",
		})
	}
	if len(unreadable) > 0 {
		sort.Strings(unreadable)
		plan.Refusals = append(plan.Refusals, rigImportRefusal{
			IDs:    unreadable,
			Reason: "the binding copy of each of these rows carries a gc.migrated_cross_edges record that does not parse, so the clear's backup cannot say what the import did with the row's boundary edges",
			Remedy: "restore each binding row's gc.migrated_cross_edges from the binding database's backup, then re-run",
		})
	}
	if len(collisions) > 0 {
		sort.Strings(collisions)
		plan.Refusals = append(plan.Refusals, rigImportRefusal{
			IDs:    collisions,
			Reason: "the city binding already holds each of these ids as a different bead",
			Remedy: "compare each binding row with the rig row (`gc bd show <id>` reads the binding first) and remove the one that is not wanted, then re-run",
		})
	}
	if len(reused) > 0 {
		sort.Strings(reused)
		plan.Refusals = append(plan.Refusals, rigImportRefusal{
			IDs:    reused,
			Reason: "the rig minted each of these ids again after an earlier import moved the bead that first held it",
			Remedy: "re-create each rig row under a new id (the import raised the rig's minimum id length so this does not recur), then re-run",
		})
	}
	if len(foreign) > 0 {
		sort.Strings(foreign)
		plan.Refusals = append(plan.Refusals, rigImportRefusal{
			IDs:    foreign,
			Reason: "these rows' gc.root_store_ref names another city or another rig, so they are not this rig's to move: the store is shared (or holds another rig's rows)",
			Remedy: "give this rig a store of its own, or move the rows to the scope that owns them, then re-run; a shared store is refused until the single-writer attestation exists",
		})
	}
	if len(claimed) > 0 {
		sort.Strings(claimed)
		plan.Refusals = append(plan.Refusals, rigImportRefusal{
			IDs:    claimed,
			Reason: "another city's agent holds these open rows (this city could not have minted the claimant and holds no session record for it), so the rig store is shared",
			Remedy: "let the other city finish or release them, or give this rig a store of its own, then re-run",
		})
	}

	record := func(id string, rec rigCrossEdgeRecord) {
		plan.Records[id] = append(plan.Records[id], rec)
	}
	statusOf := func(id string) string {
		if b, ok := snap.byID[id]; ok {
			return b.Status
		}
		if b, ok := held[id]; ok {
			return b.Status
		}
		return ""
	}
	for _, b := range snap.rows {
		for _, d := range snap.down[b.ID] {
			from, to := d.IssueID, d.DependsOnID
			if from == "" {
				from = b.ID
			}
			fromMoved, toMoved := moved[from], moved[to]
			if !fromMoved && !toMoved {
				continue
			}
			edge := beads.Dep{IssueID: from, DependsOnID: to, Type: d.Type}
			switch {
			case fromMoved && toMoved:
				plan.Copy = append(plan.Copy, edge)
			case fromMoved:
				refPlanOutEdge(&plan, ctx, edge, infra, inBinding, record, statusOf)
			default:
				refPlanInEdge(&plan, edge, snap, infra, inBinding, held, record)
			}
		}
	}
	for id := range plan.Records {
		recs := plan.Records[id]
		sort.Slice(recs, func(i, j int) bool {
			if recs[i].Dir != recs[j].Dir {
				return recs[i].Dir < recs[j].Dir
			}
			return recs[i].Other < recs[j].Other
		})
	}
	sort.Slice(plan.Copy, func(i, j int) bool {
		if plan.Copy[i].IssueID != plan.Copy[j].IssueID {
			return plan.Copy[i].IssueID < plan.Copy[j].IssueID
		}
		return plan.Copy[i].DependsOnID < plan.Copy[j].DependsOnID
	})
	sort.Strings(plan.Annotated)
	return plan
}

// refPlanOutEdge decides an edge from a moved row D onto a row that does not
// move.
func refPlanOutEdge(plan *rigImportPlan, ctx rigPlanContext, edge beads.Dep, infra, inBinding map[string]bool, record func(string, rigCrossEdgeRecord), statusOf func(string) string) {
	snap, held := ctx.Snap, ctx.Held
	d := snap.byID[edge.IssueID]
	to := edge.DependsOnID
	if w, ok := snap.byID[to]; ok && !infra[to] {
		refPlanWorkBlocker(plan, d, w, edge, record)
		return
	}
	if inBinding[to] {
		// An infrastructure row an earlier import carried, or one the
		// binding holds as this city's: both ends are binding rows.
		plan.Copy = append(plan.Copy, edge)
		return
	}
	if strings.HasPrefix(to, "external:") {
		plan.Copy = append(plan.Copy, edge)
		record(d.ID, rigCrossEdgeRecord{Dir: "out", Type: edge.Type, Other: to, Action: rigEdgeExternal})
		return
	}
	if _, ok := held[to]; ok {
		plan.Copy = append(plan.Copy, edge)
		return
	}
	refPlanElsewhereBlocker(plan, ctx, d, edge, record, statusOf)
}

// refPlanElsewhereBlocker decides D -> X where neither the rig nor the binding
// holds X: X may be held by another store (another rig, the city work store).
// A blocker held elsewhere is encoded as an S3b gate (built for exactly a
// blocker held in another store) or refused; the edge is dropped only once X
// is proven gone from every store.
func refPlanElsewhereBlocker(plan *rigImportPlan, ctx rigPlanContext, d beads.Bead, edge beads.Dep, record func(string, rigCrossEdgeRecord), statusOf func(string) string) {
	to := edge.DependsOnID
	rec := rigCrossEdgeRecord{Dir: "out", Type: edge.Type, Other: to, OtherStatus: statusOf(to), Action: rigEdgeDropped}
	blocking := beads.IsReadyBlockingDependencyType(edge.Type)
	if (!blocking && !rigMembershipEdge(edge.Type)) || !rigRowOpen(d) {
		record(d.ID, rec)
		return
	}
	copies := ctx.Elsewhere[to]
	if len(copies) == 0 {
		if len(ctx.ElsewhereUnread) > 0 {
			plan.Refusals = append(plan.Refusals, rigImportRefusal{
				IDs:    []string{d.ID, to},
				Reason: fmt.Sprintf("open %s depends (%s) on %s, which neither the rig nor the binding holds, and store(s) that may hold it could not be read (%s)", d.ID, edge.Type, to, strings.Join(ctx.ElsewhereUnread, "; ")),
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
				plan.Refusals = append(plan.Refusals, rigImportRefusal{
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
			plan.Refusals = append(plan.Refusals, rigImportRefusal{
				IDs:    []string{d.ID, to},
				Reason: fmt.Sprintf("open %s waits (%s) on unfinished %s, held in another store, and a cross-store gate encodes only blocks", d.ID, edge.Type, to),
				Remedy: fmt.Sprintf("finish %s, or close %s's workflow, then re-run", to, d.ID),
			})
			return
		}
		plan.Gates = append(plan.Gates, rigImportGate{Owner: d, Blocker: to})
		rec.Action = rigEdgeGated
		break
	}
	record(d.ID, rec)
}

// refPlanWorkBlocker decides D -> W where W is rig work that stays.
func refPlanWorkBlocker(plan *rigImportPlan, d, w beads.Bead, edge beads.Dep, record func(string, rigCrossEdgeRecord)) {
	rec := rigCrossEdgeRecord{Dir: "out", Type: edge.Type, Other: w.ID, OtherStatus: w.Status, Action: rigEdgeDropped}
	switch {
	case rigMembershipEdge(edge.Type):
		if rigRowOpen(d) && rigRowOpen(w) {
			plan.Refusals = append(plan.Refusals, rigImportRefusal{
				IDs:    []string{d.ID, w.ID},
				Reason: fmt.Sprintf("%s and rig work %s are both open and joined by a %s edge, a membership edge that cannot cross stores", d.ID, w.ID, edge.Type),
				Remedy: fmt.Sprintf("close the workflow (or the convoy) %s belongs to, or close %s, then re-run", d.ID, w.ID),
			})
			return
		}
	case beads.IsReadyBlockingDependencyType(edge.Type):
		if rigRowOpen(d) && !rigRowSatisfied(w) {
			if edge.Type != "blocks" {
				plan.Refusals = append(plan.Refusals, rigImportRefusal{
					IDs:    []string{d.ID, w.ID},
					Reason: fmt.Sprintf("open %s waits (%s) on unfinished rig work %s, and a cross-store gate encodes only blocks", d.ID, edge.Type, w.ID),
					Remedy: fmt.Sprintf("finish %s, or close %s's workflow, then re-run", w.ID, d.ID),
				})
				return
			}
			plan.Gates = append(plan.Gates, rigImportGate{Owner: d, Blocker: w.ID})
			rec.Action = rigEdgeGated
		}
	}
	record(d.ID, rec)
}

// refPlanInEdge decides an edge from a row that does not move onto a moved row D.
func refPlanInEdge(plan *rigImportPlan, edge beads.Dep, snap rigSnapshot, infra, inBinding map[string]bool, held map[string]beads.Bead, record func(string, rigCrossEdgeRecord)) {
	from := edge.IssueID
	d := snap.byID[edge.DependsOnID]
	if inBinding[from] {
		if _, ok := held[from]; ok {
			// An earlier import carried the dependent: both ends are now
			// binding rows.
			plan.Copy = append(plan.Copy, edge)
			return
		}
	}
	w, ok := snap.byID[from]
	if !ok || infra[from] {
		record(d.ID, rigCrossEdgeRecord{Dir: "in", Type: edge.Type, Other: from, Action: rigEdgeDropped})
		return
	}
	switch {
	case rigMembershipEdge(edge.Type):
		if rigRowOpen(d) && rigRowOpen(w) {
			plan.Refusals = append(plan.Refusals, rigImportRefusal{
				IDs:    []string{w.ID, d.ID},
				Reason: fmt.Sprintf("rig work %s and %s are both open and joined by a %s edge, a membership edge that cannot cross stores", w.ID, d.ID, edge.Type),
				Remedy: fmt.Sprintf("close the workflow (or the convoy) %s belongs to, or close %s, then re-run", d.ID, w.ID),
			})
			return
		}
	case beads.IsReadyBlockingDependencyType(edge.Type):
		if rigRowOpen(w) && !rigRowSatisfied(d) {
			plan.Refusals = append(plan.Refusals, rigImportRefusal{
				IDs:    []string{w.ID, d.ID},
				Reason: fmt.Sprintf("open rig work %s is blocked (%s) by open %s, and a work store cannot hold a cross-store gate", w.ID, edge.Type, d.ID),
				Remedy: fmt.Sprintf("finish or cancel %s's workflow, or remove the edge with `gc bd dep remove %s %s`, then re-run", d.ID, w.ID, d.ID),
			})
			return
		}
	}
	record(d.ID, rigCrossEdgeRecord{Dir: "in", Type: edge.Type, Other: w.ID, OtherStatus: w.Status, Action: rigEdgeDropped})
}
