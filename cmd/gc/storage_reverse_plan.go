package main

// The reverse migration's plan (S5 §4.2): where each binding row goes, which
// edges go with it, and which shapes refuse. Nothing here reads or writes a
// store: planReverse is a pure function of the binding's rows, edges and
// payloads, the rig import manifests, and what each destination already
// holds, so `--dry-run` prints exactly the plan the run copies by.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/dispatch"
)

// reverseCityDest names the city work store as a destination; a rig's is
// rigImportStamp(rig), the stamp its import wrote.
const reverseCityDest = "city"

// reverseMode is what one reverse moves.
type reverseMode string

const (
	// reverseToWork moves every binding row back (S5 §4): the city leaves
	// the split.
	reverseToWork reverseMode = "to-work"
	// reverseToRigs moves the named rigs' imported rows back to them (S4c);
	// the city stays split.
	reverseToRigs reverseMode = "to-rigs"
)

// reverseInput is everything planReverse reads.
type reverseInput struct {
	Mode reverseMode
	// Rigs are the rigs a to-rigs reverse moves.
	Rigs map[string]bool
	// Stamp is this run's gc.infra_reversed_from value.
	Stamp string
	// Binding is every row the binding holds, both tiers, closed included.
	Binding []beads.Bead
	// Down is every binding row's edges, keyed by anchor.
	Down map[string][]beads.Dep
	// Payloads holds the payload of every binding edge that carries one.
	Payloads map[beads.DepPair]string
	// Manifests are the rig import manifests, by rig; nil for a rig with no
	// import marker.
	Manifests map[string]*infraCopyManifest
	// Configured names the rigs with a store of their own.
	Configured map[string]bool
	// Held is what each destination store already holds, by id.
	Held map[string]map[string]beads.Bead
	// CarriesPayload reports whether a destination can write an edge payload.
	CarriesPayload map[string]bool
	// DerivesParent names the destinations that hold a parent as a
	// parent-child edge, not a field (beads.CreatedFormer: native Dolt).
	DerivesParent map[string]bool
	// DropsRef names the destinations that store no step ref.
	DropsRef map[string]bool
	// Fresh is set on a run's first plan: a destination already holding a
	// row under its stamp is refused.
	Fresh bool
}

// reversePlan is what one reverse writes.
type reversePlan struct {
	// Rows are the rows each destination receives, in their reversed form,
	// sorted by id.
	Rows map[string][]beads.Bead
	// Edges are the edges each destination receives, sorted: those between
	// two rows that land there, external: references, and the recorded cross
	// edges restored.
	Edges map[string][]beads.DepWithPayload
	// Leaving are the binding rows a to-rigs reverse removes from the
	// binding once their copies are proven: the moved rows and the closed
	// gates no row the binding keeps waits on (reverseClosedGates).
	Leaving []string
	// Restored counts the recorded cross edges restored.
	Restored int
	// Dropped names each edge not carried, with why.
	Dropped []string
	// Gates counts the closed cross-store gates not carried that go with the
	// reverse: on a to-rigs, those that leave the binding.
	Gates int
	// RefsKept counts the rows whose step ref a destination does not store,
	// kept in gc.step_ref.
	RefsKept int
	// RefsDropped counts the roots whose ref (their formula's name) a
	// destination does not store.
	RefsDropped int
	// BindingIDs is every id the binding held when the plan was read: a
	// prepare never deletes a stamped row whose binding copy is gone.
	BindingIDs map[string]bool
	// Leftovers are, by destination, the stranded rows the store holds that
	// the reverse leaves where they are (reverseRun.storeLeftovers).
	Leftovers map[string]reverseLeftovers
	// StrandsUnchecked records that the binding converged before its copy
	// manifest was recorded, so a --to-work could not check the work store
	// for rows the binding does not serve.
	StrandsUnchecked bool
	Refusals         []rigImportRefusal
}

// reverseLeftovers are the stranded rows of one store that a reverse leaves
// there as they are, where a store that serves every class keeps them.
type reverseLeftovers struct {
	// AsWork maps each row that is infrastructure class only because of
	// metadata to those keys.
	AsWork map[string][]string
	// Closed are the closed ones.
	Closed []string
	// Unread are a rig's open mail and nudge rows (messaging and nudges
	// class): mail, external messaging and the nudge queue read only the
	// city's store (resolveMailMessagesStore, newCityExtMsgServices,
	// openNudgeBeadStoreOwned), split or not, and no relic leg covers those
	// classes, so nothing delivers them from a rig's store.
	Unread []string
}

// reverseRow returns b as a reverse writes it: the forward stamp, the record
// of the boundary edges and, for a row whose scope its rig import added, that
// scope removed; this run's stamp added; the create-time edge shorthands and
// the readiness projection stripped.
func reverseRow(b beads.Bead, stamp string, annotated bool) beads.Bead {
	metadata := make(beads.StringMap, len(b.Metadata))
	for key, value := range b.Metadata {
		metadata[key] = value
	}
	delete(metadata, infraMigrationStampKey)
	delete(metadata, beadmeta.MigratedCrossEdgesMetadataKey)
	if annotated {
		delete(metadata, beadmeta.RootStoreRefMetadataKey)
	}
	metadata[beadmeta.InfraReversedFromMetadataKey] = stamp
	b.Metadata = metadata
	b.Dependencies, b.Needs, b.IsBlocked = nil, nil, nil
	return b
}

// isCrossStoreGate reports whether b is an S3b cross-store gate.
func isCrossStoreGate(b beads.Bead) bool {
	return hasLabel(b.Labels, dispatch.CrossStoreGateLabel)
}

// reverseReturns reports whether a to-rigs reverse returns b: a row stamped
// as the import of a rig it names.
func reverseReturns(in reverseInput, b beads.Bead) bool {
	rig, imported := strings.CutPrefix(b.Metadata[infraMigrationStampKey], rigImportStamp(""))
	return imported && rig != "" && in.Rigs[rig]
}

// reverseRoute says where one binding row goes: dest ("" when it stays),
// skip when it is a closed gate the reverse does not carry, or a refusal.
// byID is every binding row by id. A to-rigs reverse moves the rows it
// returns, and judges a cross-store gate with its owner: a gate the runtime
// minted for an imported row carries no import stamp
// (dispatch.NewCrossStoreGate). A closed one leaves the binding only once no
// row it keeps waits on it (reverseClosedGates).
func reverseRoute(in reverseInput, byID map[string]beads.Bead, b beads.Bead) (dest string, skip bool, refusal string) {
	stamp := b.Metadata[infraMigrationStampKey]
	rig, imported := strings.CutPrefix(stamp, rigImportStamp(""))
	imported = imported && rig != ""
	gate := isCrossStoreGate(b)
	if in.Mode == reverseToRigs && !reverseReturns(in, b) && (!gate || !reverseReturns(in, byID[b.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey]])) {
		return "", false, ""
	}
	if gate {
		if rigRowOpen(b) {
			return "", false, "open-gate"
		}
		return "", true, ""
	}
	if imported {
		return rigImportStamp(rig), false, ""
	}
	if ref := strings.TrimSpace(b.Metadata[beadmeta.RootStoreRefMetadataKey]); strings.HasPrefix(ref, rigImportStamp("")) && stamp != infraMigrationCityStamp {
		if rigRowOpen(b) {
			return "", false, "d8"
		}
	}
	return reverseCityDest, false, ""
}

// infraMigrationCityStamp is the stamp the city copy writes (infraMigrationRow).
const infraMigrationCityStamp = "work"

// planReverse plans one reverse.
func planReverse(in reverseInput) reversePlan {
	plan := reversePlan{Rows: map[string][]beads.Bead{}, Edges: map[string][]beads.DepWithPayload{}}
	byID := make(map[string]beads.Bead, len(in.Binding))
	destOf := map[string]string{}
	carried := map[string]bool{} // rows that move
	closedGate := map[string]bool{}
	refused := map[string]bool{} // rows refused on their own: their edges are not judged again
	leaving := map[string]bool{}
	var openGates, d8, unconfigured, unrecorded, reused, unreadable, stampTaken []string
	for _, b := range in.Binding {
		byID[b.ID] = b
	}
	for _, b := range sortedBeads(in.Binding) {
		dest, skip, refusal := reverseRoute(in, byID, b)
		if refusal != "" {
			refused[b.ID] = true
		}
		switch {
		case refusal == "open-gate":
			openGates = append(openGates, fmt.Sprintf("%s (gate for %s on %s)", b.ID, b.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey], b.Metadata[beadmeta.CrossStoreBlockerMetadataKey]))
			continue
		case refusal == "d8":
			d8 = append(d8, fmt.Sprintf("%s (%s, %s)", b.ID, b.Metadata[beadmeta.RootStoreRefMetadataKey], b.Status))
			continue
		case skip:
			closedGate[b.ID] = true
			continue
		case dest == "":
			continue
		}
		annotated := false
		if rig, ok := strings.CutPrefix(dest, rigImportStamp("")); ok {
			manifest := in.Manifests[rig]
			switch {
			case !in.Configured[rig]:
				unconfigured = append(unconfigured, fmt.Sprintf("%s (rig %s)", b.ID, rig))
				continue
			case manifest == nil || !manifest.IDs[b.ID]:
				unrecorded = append(unrecorded, fmt.Sprintf("%s (rig %s)", b.ID, rig))
				continue
			}
			annotated = manifest.ScopeAnnotated[b.ID]
		}
		if held, ok := in.Held[dest][b.ID]; ok && held.Metadata[beadmeta.InfraReversedFromMetadataKey] != in.Stamp {
			reused = append(reused, fmt.Sprintf("%s (%s)", b.ID, reverseDestNoun(dest)))
			continue
		}
		destOf[b.ID] = dest
		carried[b.ID] = true
		row := reverseRow(b, in.Stamp, annotated)
		if in.DropsRef[dest] && row.Ref != "" && row.Metadata[beadmeta.StepRefMetadataKey] == "" {
			// A root's ref is its formula's name, not a step's: it is not
			// copied into gc.step_ref, and only counted.
			if root := row.Metadata[beadmeta.RootBeadIDMetadataKey]; root == "" || root == row.ID {
				plan.RefsDropped++
			} else {
				row.Metadata[beadmeta.StepRefMetadataKey] = row.Ref
				plan.RefsKept++
			}
		}
		plan.Rows[dest] = append(plan.Rows[dest], row)
		if in.Mode == reverseToRigs {
			leaving[b.ID] = true
		}
	}
	waits, gone := reverseClosedGates(in, closedGate, refused, destOf)
	plan.Gates = len(gone)
	if in.Mode == reverseToRigs {
		for _, id := range gone {
			leaving[id] = true
		}
	}
	plan.Leaving = sortedMapKeys(leaving)
	if in.Fresh {
		for _, dest := range sortedMapKeys(in.Held) {
			for _, id := range sortedMapKeys(in.Held[dest]) {
				if in.Held[dest][id].Metadata[beadmeta.InfraReversedFromMetadataKey] == in.Stamp {
					stampTaken = append(stampTaken, fmt.Sprintf("%s (%s)", id, reverseDestNoun(dest)))
				}
			}
		}
	}
	// recordsFor collects, per moved row, the edges the reverse does not
	// carry, as gc.migrated_cross_edges on the row it writes, so a later
	// import can restore them.
	recordsFor := map[string][]rigCrossEdgeRecord{}
	statusOf := func(id string) string {
		if b, ok := byID[id]; ok {
			return b.Status
		}
		for _, dest := range sortedMapKeys(in.Held) {
			if b, ok := in.Held[dest][id]; ok {
				return b.Status
			}
		}
		return ""
	}
	record := func(on string, d beads.Dep, payload string) {
		r := rigCrossEdgeRecord{Dir: "out", Type: d.Type, Other: d.DependsOnID, Payload: payload, Action: rigEdgeDropped}
		if d.DependsOnID == on {
			r.Dir, r.Other = "in", d.IssueID
		}
		r.OtherStatus = statusOf(r.Other)
		recordsFor[on] = append(recordsFor[on], r)
	}

	// whereIs is where an id lives once the reverse is done, and its row
	// there: a moved row, a row the binding keeps, or a row a destination
	// already holds, prefer's first.
	whereIs := func(id, prefer string) (string, beads.Bead, bool) {
		if dest, ok := destOf[id]; ok {
			return dest, byID[id], true
		}
		if b, ok := byID[id]; ok {
			if closedGate[id] {
				return "", beads.Bead{}, false
			}
			return "binding", b, true
		}
		if b, ok := in.Held[prefer][id]; ok {
			return prefer, b, true
		}
		for _, dest := range sortedMapKeys(in.Held) {
			if b, ok := in.Held[dest][id]; ok {
				return dest, b, true
			}
		}
		return "", beads.Bead{}, false
	}
	type edgeKey struct{ from, to string }
	added := map[string]map[edgeKey]bool{}
	var crossing, held, payloadBlocked []string
	addEdge := func(dest string, d beads.Dep, payload string) {
		if added[dest] == nil {
			added[dest] = map[edgeKey]bool{}
		}
		key := edgeKey{d.IssueID, d.DependsOnID}
		if added[dest][key] {
			return
		}
		if payload != "" && !in.CarriesPayload[dest] {
			payloadBlocked = append(payloadBlocked, fmt.Sprintf("%s -[%s]-> %s (%s)", d.IssueID, d.Type, d.DependsOnID, reverseDestNoun(dest)))
			return
		}
		added[dest][key] = true
		plan.Edges[dest] = append(plan.Edges[dest], beads.DepWithPayload{Dep: d, Payload: payload})
	}
	// judge refuses an edge that would cross stores while it still joins its
	// rows (fromOpen, and toHolds from reverseEdgeHolds), listing a
	// ready-blocking edge apart from the others, and drops and records any
	// other edge.
	judge := func(d beads.Dep, payload, fromDest, toDest string, fromOpen, toHolds bool, why string) {
		edge := fmt.Sprintf("%s -[%s]-> %s", d.IssueID, d.Type, d.DependsOnID)
		if fromOpen && toHolds {
			line := fmt.Sprintf("%s (%s, %s)", edge, reverseDestNoun(fromDest), reverseDestNoun(toDest))
			if beads.IsReadyBlockingDependencyType(d.Type) {
				held = append(held, line)
			} else {
				crossing = append(crossing, line)
			}
			return
		}
		plan.Dropped = append(plan.Dropped, edge+": "+why)
		for _, end := range []string{d.IssueID, d.DependsOnID} {
			if carried[end] {
				record(end, d, payload)
			}
		}
	}

	// The binding's live edges.
	for _, id := range sortedMapKeys(in.Down) {
		from, ok := byID[id]
		if !ok || closedGate[id] || refused[id] {
			continue
		}
		fromDest, moving := destOf[id]
		for _, d := range in.Down[id] {
			d.IssueID = id
			to := d.DependsOnID
			if refused[to] {
				continue
			}
			if closedGate[to] {
				// The gate is not carried; the edge it stood for, from this
				// row onto the gate's blocker, is judged below.
				continue
			}
			payload := in.Payloads[beads.DepPair{IssueID: id, DependsOnID: to}]
			if strings.HasPrefix(to, "external:") {
				if moving {
					addEdge(fromDest, d, payload)
				}
				continue
			}
			toDest, toRow, known := whereIs(to, fromDest)
			_, toMoving := destOf[to]
			switch {
			case !moving && !toMoving:
				continue
			case !known:
				if moving {
					plan.Dropped = append(plan.Dropped, fmt.Sprintf("%s -[%s]-> %s: no store holds %s", id, d.Type, to, to))
					record(id, d, payload)
				}
			case moving && toDest == fromDest:
				addEdge(fromDest, d, payload)
			case moving:
				judge(d, payload, fromDest, toDest, rigRowOpen(from), reverseEdgeHolds(d, toRow), "it would cross stores, and one side is closed")
			default:
				// A row the binding keeps onto one that leaves (to-rigs).
				judge(d, payload, "binding", toDest, rigRowOpen(from), reverseEdgeHolds(d, toRow), "it would cross stores, and one side is closed")
			}
		}
	}

	// overrides are the edges a closed gate stood for whose restore would
	// undo an operator's close of the gate (reverseUndoesOverride): refused.
	overrides := reverseOverrides{}
	gatesFor := reverseGatesFor(byID, closedGate)

	// The recorded cross edges (gc.migrated_cross_edges), restored where both
	// ends now share a store; a gated one whose restore would undo an
	// operator's close of its gate refuses instead.
	for _, id := range sortedMapKeys(destOf) {
		raw := strings.TrimSpace(byID[id].Metadata[beadmeta.MigratedCrossEdgesMetadataKey])
		if raw == "" {
			continue
		}
		var records []rigCrossEdgeRecord
		if err := json.Unmarshal([]byte(raw), &records); err != nil {
			unreadable = append(unreadable, fmt.Sprintf("%s (%v)", id, err))
			continue
		}
		dest := destOf[id]
		for _, r := range records {
			if r.Action == rigEdgeExternal || r.Action == rigEdgeCopied || strings.HasPrefix(r.Other, "external:") {
				continue
			}
			d := beads.Dep{IssueID: id, DependsOnID: r.Other, Type: r.Type}
			if r.Dir == "in" {
				d = beads.Dep{IssueID: r.Other, DependsOnID: id, Type: r.Type}
			}
			otherDest, other, known := whereIs(r.Other, dest)
			if !known || otherDest != dest {
				plan.Dropped = append(plan.Dropped, fmt.Sprintf("%s -[%s]-> %s: recorded on %s, and its other end is not in %s", d.IssueID, d.Type, d.DependsOnID, id, reverseDestNoun(dest)))
				recordsFor[id] = append(recordsFor[id], r)
				continue
			}
			if r.Action == rigEdgeGated && overrides.undone(byID, gatesFor[beads.DepPair{IssueID: id, DependsOnID: r.Other}], d, other) {
				continue
			}
			before := len(plan.Edges[dest])
			addEdge(dest, d, r.Payload)
			if len(plan.Edges[dest]) > before {
				plan.Restored++
			}
		}
	}

	// The records the city clear wrote on the work beads it left behind
	// (infraDroppedCrossEdgeRecords): an edge from a row a destination holds
	// onto a row that now returns to it. The row keeps its record: the
	// reverse writes no row it did not copy.
	for _, dest := range sortedMapKeys(in.Held) {
		for _, id := range sortedMapKeys(in.Held[dest]) {
			raw := strings.TrimSpace(in.Held[dest][id].Metadata[beadmeta.MigratedCrossEdgesMetadataKey])
			if raw == "" || destOf[id] != "" {
				continue
			}
			var records []rigCrossEdgeRecord
			if err := json.Unmarshal([]byte(raw), &records); err != nil {
				unreadable = append(unreadable, fmt.Sprintf("%s (%v)", id, err))
				continue
			}
			for _, r := range records {
				if destOf[r.Other] != dest || r.Action == rigEdgeExternal || r.Action == rigEdgeCopied {
					continue
				}
				d := beads.Dep{IssueID: id, DependsOnID: r.Other, Type: r.Type}
				if r.Dir == "in" {
					d = beads.Dep{IssueID: r.Other, DependsOnID: id, Type: r.Type}
				}
				before := len(plan.Edges[dest])
				addEdge(dest, d, r.Payload)
				if len(plan.Edges[dest]) > before {
					plan.Restored++
				}
			}
		}
	}

	// A closed cross-store gate is not carried, but the edge it stood for is:
	// each moved row that waited on it gets row -> blocker, from the gate's
	// own metadata (reverseGateEdges), where it lands with the blocker. A
	// gate's owner waits on it only when a migration minted it; a drain's
	// control, which owns the gates the runtime mints (S3b), waits on its
	// items, so it gets no edge. Where the blocker lands in another store, or
	// in none, the row records the edge as one the reverse drops, unless its
	// record already holds it (a migration's gate: its owner's record, above).
	// An edge whose restore would undo an operator's close of its gate
	// refuses instead, as its record does.
	for _, e := range reverseGateEdges(byID, waits, destOf) {
		blockerDest, blocker, known := whereIs(e.dep.DependsOnID, e.dest)
		switch {
		case known && blockerDest == e.dest && overrides.undone(byID, []string{e.gate}, e.dep, blocker):
			// Refused below.
		case known && blockerDest == e.dest:
			before := len(plan.Edges[e.dest])
			addEdge(e.dest, e.dep, "")
			if len(plan.Edges[e.dest]) > before {
				plan.Restored++
			}
		case reverseRecordsEdge(recordsFor[e.dep.IssueID], e.dep):
		default:
			why := "no store holds " + e.dep.DependsOnID
			if known {
				why = "it would cross stores (" + reverseDestNoun(blockerDest) + ")"
			}
			plan.Dropped = append(plan.Dropped, fmt.Sprintf("%s -[%s]-> %s: the edge closed gate %s stood for, and %s", e.dep.IssueID, e.dep.Type, e.dep.DependsOnID, e.gate, why))
			record(e.dep.IssueID, e.dep, "")
		}
	}

	// A store that holds a parent as a parent-child edge (native Dolt) reads a
	// row's parent from that edge alone. So each moved row's parent becomes
	// the edge where the parent lands with it, a row with only the edge gets
	// the parent, and a parent that lands elsewhere is not one the row keeps:
	// judged as the edge it would be, and recorded.
	for _, dest := range sortedMapKeys(plan.Rows) {
		if !in.DerivesParent[dest] {
			continue
		}
		parents := map[string][]string{}
		for _, e := range plan.Edges[dest] {
			if e.Type == "parent-child" {
				parents[e.IssueID] = append(parents[e.IssueID], e.DependsOnID)
			}
		}
		for i := range plan.Rows[dest] {
			row := &plan.Rows[dest][i]
			switch {
			case row.ParentID == "" && len(parents[row.ID]) == 1:
				row.ParentID = parents[row.ID][0]
			case row.ParentID == "" || reverseHasID(parents[row.ID], row.ParentID):
			default:
				d := beads.Dep{IssueID: row.ID, DependsOnID: row.ParentID, Type: "parent-child"}
				parentDest, parent, known := whereIs(row.ParentID, dest)
				switch {
				case known && parentDest == dest:
					addEdge(dest, d, "")
				case known:
					judge(d, "", dest, parentDest, rigRowOpen(*row), rigRowOpen(parent), "its parent would be in another store, and one side is closed")
					row.ParentID = ""
				default:
					plan.Dropped = append(plan.Dropped, fmt.Sprintf("%s -[parent-child]-> %s: no store holds the parent", d.IssueID, d.DependsOnID))
					record(row.ID, d, "")
					row.ParentID = ""
				}
			}
		}
	}
	for dest, rows := range plan.Rows {
		for i := range rows {
			if recs := recordsFor[rows[i].ID]; len(recs) > 0 {
				if raw, err := json.Marshal(recs); err == nil {
					rows[i].Metadata[beadmeta.MigratedCrossEdgesMetadataKey] = string(raw)
				}
			}
		}
		plan.Rows[dest] = rows
	}

	refuse := func(ids []string, reason, remedy string) {
		if len(ids) == 0 {
			return
		}
		sort.Strings(ids)
		plan.Refusals = append(plan.Refusals, rigImportRefusal{IDs: ids, Reason: reason, Remedy: remedy})
	}
	refuse(stampTaken, "a store already holds each of these rows under this run's stamp, so an abort of this run could not tell them apart",
		"run the reverse again: a new run takes a new stamp")
	refuse(openGates, "each is an open cross-store gate: a single store has no release lane, so the gate would hold its owner forever",
		"release it (close the work it waits on) or cancel the owner's workflow (`gc workflow delete <root>`), then re-run")
	refuse(sortedMapKeys(overrides), "an operator closed each gate, releasing the open row that waited on it whatever the blocker does, and a single store has no gate to keep that: the row would land with its unfinished blocker and wait on it again",
		"finish or close the blocker, or close the row, then re-run; to let a drain's item run while its blocker stays open, remove each listed row's edge onto the gate and the drain's convoy member's edge onto the blocker (a drain on one store projects that edge onto its item on every pass)")
	refuse(d8, "each is an open row the binding minted for a rig's scope: on a single-store city no reader reaches it there (D8)",
		"finish or cancel its workflow (`gc workflow delete <root>`), then re-run; closed ones move to the city store")
	refuse(unconfigured, "each was imported from a rig this city no longer configures, so there is no store to return it to",
		"add the rig back to city.toml, then re-run")
	refuse(unrecorded, "each is stamped as a rig's import but the rig's import manifest does not record it: the import was interrupted",
		"finish the import with `"+rigImportAllInstruction()+"`, then re-run")
	refuse(reused, "the destination already holds each of these ids as another bead",
		"compare the two rows (`gc bd show <id>` reads the binding first) and remove the one that is not wanted from its store, then re-run")
	refuse(unreadable, "each carries a gc.migrated_cross_edges record that does not parse, so its boundary edges cannot be restored",
		"restore the row's gc.migrated_cross_edges from a snapshot of the binding, then re-run")
	refuse(crossing, "each edge joins two open rows that would land in different stores, and no cross-store edge is ever written",
		"close one side (finish or cancel it), then re-run")
	refuse(held, "each edge holds an open row behind a blocker that has not released it (open, or closed with gc.work_outcome=blocked), and the two would land in different stores: no cross-store edge is ever written, and dropping the edge would release the row",
		"finish the blocker, or close the dependent, then re-run (a blocker closed blocked releases its dependents only once its work is done and gc.work_outcome no longer says blocked)")
	refuse(payloadBlocked, "each edge carries a payload and its destination store cannot write one (native Dolt has no edge-payload writer yet)",
		"keep the city split until a build whose store carries edge payloads, or close and remove the edge if it is no longer needed; nothing was written")
	for dest := range plan.Edges {
		sort.Slice(plan.Edges[dest], func(i, j int) bool {
			a, b := plan.Edges[dest][i], plan.Edges[dest][j]
			if a.IssueID != b.IssueID {
				return a.IssueID < b.IssueID
			}
			return a.DependsOnID < b.DependsOnID
		})
	}
	sort.Strings(plan.Dropped)
	return plan
}

// reverseDestNoun names a destination for an operator.
func reverseDestNoun(dest string) string {
	switch {
	case dest == reverseCityDest:
		return "the city work store"
	case dest == "binding":
		return "the binding"
	case strings.HasPrefix(dest, rigImportStamp("")):
		return "the store of rig " + strings.TrimPrefix(dest, rigImportStamp(""))
	}
	return dest
}

// reverseHasID reports whether ids holds id.
func reverseHasID(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// reverseRecordsEdge reports whether records, a row's cross-edge records, hold
// d, an edge out of that row.
func reverseRecordsEdge(records []rigCrossEdgeRecord, d beads.Dep) bool {
	for _, r := range records {
		if r.Dir == "out" && r.Type == d.Type && r.Other == d.DependsOnID {
			return true
		}
	}
	return false
}

// reverseClosedGates sorts the closed cross-store gates a plan does not carry
// (closed) by the rows that wait on them. waits maps each to the edges onto it
// from the binding's rows, a refused row's aside (its edges are not judged),
// and gone names those that go with the reverse: every one on a --to-work,
// which retires the binding, and on a --to-rigs each one no row the binding
// keeps (one destOf does not route) waits on. A gate a kept row waits on
// stays in the binding with it, closed: the release lane retires a released
// gate whose owner its store no longer holds live.
func reverseClosedGates(in reverseInput, closed, refused map[string]bool, destOf map[string]string) (map[string][]beads.Dep, []string) {
	waits := map[string][]beads.Dep{}
	for _, id := range sortedMapKeys(in.Down) {
		if refused[id] || closed[id] {
			continue
		}
		for _, d := range in.Down[id] {
			if closed[d.DependsOnID] {
				d.IssueID = id
				waits[d.DependsOnID] = append(waits[d.DependsOnID], d)
			}
		}
	}
	kept := map[string]bool{}
	if in.Mode == reverseToRigs {
		kept = reverseKeptGates(in.Down, closed, func(id string) bool { return !refused[id] && destOf[id] == "" })
	}
	var gone []string
	for _, id := range sortedMapKeys(closed) {
		if !kept[id] {
			gone = append(gone, id)
		}
	}
	return waits, gone
}

// reverseKeptGates is each of gates that a row staying in the binding (stays)
// waits on, from down: a to-rigs reverse keeps such a gate with that row. The
// plan (reverseClosedGates) and the clear (reverseWaitedGates) both judge
// with it, so the gates the plan leaves are the gates the clear removes.
func reverseKeptGates(down map[string][]beads.Dep, gates map[string]bool, stays func(string) bool) map[string]bool {
	kept := map[string]bool{}
	for id, deps := range down {
		if gates[id] || !stays(id) {
			continue
		}
		for _, d := range deps {
			if gates[d.DependsOnID] {
				kept[d.DependsOnID] = true
			}
		}
	}
	return kept
}

// reverseGateEdge is the edge a closed cross-store gate stood for, for one
// moved row that waited on it: the row's edge onto the gate, its type kept,
// pointed at the gate's blocker. It carries no payload: every writer of an
// edge onto a gate (the drain's projection, ensureBlockingDependency; the
// migrations' createRigImportGate callers) adds a plain blocks edge.
type reverseGateEdge struct {
	gate, dest string // the gate, and where the row lands
	dep        beads.Dep
}

// reverseGateEdges is the edge each gate of waits (reverseClosedGates) stood
// for, for each row that waits on it and moves (destOf), by gate. A gate
// that names no blocker stands for no other edge than the row's own onto it.
func reverseGateEdges(byID map[string]beads.Bead, waits map[string][]beads.Dep, destOf map[string]string) []reverseGateEdge {
	var out []reverseGateEdge
	for _, gate := range sortedMapKeys(waits) {
		blocker := strings.TrimSpace(byID[gate].Metadata[beadmeta.CrossStoreBlockerMetadataKey])
		for _, w := range waits[gate] {
			dest, moving := destOf[w.IssueID]
			if !moving {
				continue
			}
			d := w
			if blocker != "" {
				d.DependsOnID = blocker
			}
			out = append(out, reverseGateEdge{gate: gate, dest: dest, dep: d})
		}
	}
	return out
}

// reverseGatesFor indexes the closed gates a plan does not carry (closed) by
// the edge each stands for, owner -> blocker: the edge a migration's gate
// stands for is recorded on its owner as gated.
func reverseGatesFor(byID map[string]beads.Bead, closed map[string]bool) map[beads.DepPair][]string {
	out := map[beads.DepPair][]string{}
	for _, id := range sortedMapKeys(closed) {
		meta := byID[id].Metadata
		key := beads.DepPair{IssueID: strings.TrimSpace(meta[beadmeta.CrossStoreGateOwnerMetadataKey]), DependsOnID: strings.TrimSpace(meta[beadmeta.CrossStoreBlockerMetadataKey])}
		out[key] = append(out[key], id)
	}
	return out
}

// reverseEdgeHolds reports whether to, the row d points at, still holds d's row
// to it: for a ready-blocking edge, a blocker that has not released its
// dependents (open, or closed blocked: rigRowSatisfied), as readiness and the
// forward migration judge it; for any other edge, an open row.
func reverseEdgeHolds(d beads.Dep, to beads.Bead) bool {
	if beads.IsReadyBlockingDependencyType(d.Type) {
		return !rigRowSatisfied(to)
	}
	return rigRowOpen(to)
}

// reverseUndoesOverride reports whether restoring d, row -> blocker, the edge
// that gate (a closed cross-store gate) stood for, would hold row again
// although the gate released it for good: an operator closed the gate, a close
// the runtime never takes back (dispatch.CrossStoreGateStateOverridden),
// unlike the controller's release, retire or teardown close
// (dispatch.IsControllerClosedCrossStoreGate), which a live drain takes back
// once the blocker holds again; d blocks; row is open; and blocker, the row it
// lands with, does not release it. A single store has no gate to keep such a
// release, and a live drain there projects its member's edge onto its item
// again on every pass, so the reverse refuses the edge rather than drop it.
func reverseUndoesOverride(gate beads.Bead, d beads.Dep, row, blocker beads.Bead) bool {
	return !dispatch.IsControllerClosedCrossStoreGate(gate) && beads.IsReadyBlockingDependencyType(d.Type) && rigRowOpen(row) && !rigRowSatisfied(blocker)
}

// reverseOverrides are the refusal lines of the edges a plan does not restore
// because their restore would undo an operator's close of the gate that stood
// for them, each once: a migration's gate meets its owner's gated record and
// its owner's edge onto it.
type reverseOverrides map[string]bool

// undone reports whether restoring d, the edge one of gates stood for, onto
// blocker as it lands with d's row, would undo an operator's close of that
// gate (reverseUndoesOverride), and records d when it would.
func (o reverseOverrides) undone(byID map[string]beads.Bead, gates []string, d beads.Dep, blocker beads.Bead) bool {
	for _, gate := range gates {
		if reverseUndoesOverride(byID[gate], d, byID[d.IssueID], blocker) {
			o[fmt.Sprintf("%s -[%s]-> %s (gate %s)", d.IssueID, d.Type, d.DependsOnID, gate)] = true
			return true
		}
	}
	return false
}

// sortedBeads returns rows sorted by id, without mutating the caller's slice.
func sortedBeads(rows []beads.Bead) []beads.Bead {
	out := append([]beads.Bead(nil), rows...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
