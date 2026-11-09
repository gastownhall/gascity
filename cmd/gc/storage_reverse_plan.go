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
	// binding once their copies are proven: the moved rows and their rig's
	// closed gates.
	Leaving []string
	// Restored counts the recorded cross edges restored.
	Restored int
	// Dropped names each edge not carried, with why.
	Dropped []string
	// Gates counts the closed cross-store gates not carried.
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
	Refusals   []rigImportRefusal
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

// reverseRoute says where one binding row goes: dest ("" when it stays),
// skip when it is a closed gate the reverse does not carry, or a refusal.
func reverseRoute(in reverseInput, b beads.Bead) (dest string, skip bool, refusal string) {
	stamp := b.Metadata[infraMigrationStampKey]
	rig, imported := strings.CutPrefix(stamp, rigImportStamp(""))
	imported = imported && rig != ""
	if in.Mode == reverseToRigs && (!imported || !in.Rigs[rig]) {
		return "", false, ""
	}
	if isCrossStoreGate(b) {
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
		dest, skip, refusal := reverseRoute(in, b)
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
			plan.Gates++
			if in.Mode == reverseToRigs {
				leaving[b.ID] = true
			}
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

	// whereIs is where an id lives once the reverse is done, and whether it
	// is known open: a moved row, a row the binding keeps, or a row a
	// destination already holds, prefer's first.
	whereIs := func(id, prefer string) (string, bool, bool) {
		if dest, ok := destOf[id]; ok {
			return dest, rigRowOpen(byID[id]), true
		}
		if b, ok := byID[id]; ok {
			if closedGate[id] {
				return "", false, false
			}
			return "binding", rigRowOpen(b), true
		}
		if b, ok := in.Held[prefer][id]; ok {
			return prefer, rigRowOpen(b), true
		}
		for _, dest := range sortedMapKeys(in.Held) {
			if b, ok := in.Held[dest][id]; ok {
				return dest, rigRowOpen(b), true
			}
		}
		return "", false, false
	}
	type edgeKey struct{ from, to string }
	added := map[string]map[edgeKey]bool{}
	var crossing, payloadBlocked []string
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
	judge := func(d beads.Dep, payload, fromDest, toDest string, fromOpen, toOpen bool, why string) {
		edge := fmt.Sprintf("%s -[%s]-> %s", d.IssueID, d.Type, d.DependsOnID)
		if fromOpen && toOpen {
			crossing = append(crossing, fmt.Sprintf("%s (%s, %s)", edge, reverseDestNoun(fromDest), reverseDestNoun(toDest)))
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
				// The gate is not carried; its record on its owner restores
				// the edge it stood for (below).
				continue
			}
			payload := in.Payloads[beads.DepPair{IssueID: id, DependsOnID: to}]
			if strings.HasPrefix(to, "external:") {
				if moving {
					addEdge(fromDest, d, payload)
				}
				continue
			}
			toDest, toOpen, known := whereIs(to, fromDest)
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
				judge(d, payload, fromDest, toDest, rigRowOpen(from), toOpen, "it would cross stores, and one side is closed")
			default:
				// A row the binding keeps onto one that leaves (to-rigs).
				judge(d, payload, "binding", toDest, rigRowOpen(from), toOpen, "it would cross stores, and one side is closed")
			}
		}
	}

	// The recorded cross edges (gc.migrated_cross_edges), restored where both
	// ends now share a store.
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
			otherDest, _, known := whereIs(r.Other, dest)
			if !known || otherDest != dest {
				plan.Dropped = append(plan.Dropped, fmt.Sprintf("%s -[%s]-> %s: recorded on %s, and its other end is not in %s", d.IssueID, d.Type, d.DependsOnID, id, reverseDestNoun(dest)))
				recordsFor[id] = append(recordsFor[id], r)
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
	// its owner -> blocker, from the gate's own metadata, where both ends
	// share a store. A gate a migration minted has a record on its owner too
	// (above); a gate the runtime minted (S3b) has only its metadata.
	for _, id := range sortedMapKeys(closedGate) {
		owner, blocker := byID[id].Metadata[beadmeta.CrossStoreGateOwnerMetadataKey], byID[id].Metadata[beadmeta.CrossStoreBlockerMetadataKey]
		dest, moving := destOf[owner]
		if !moving || blocker == "" {
			continue
		}
		if blockerDest, _, known := whereIs(blocker, dest); known && blockerDest == dest {
			before := len(plan.Edges[dest])
			addEdge(dest, beads.Dep{IssueID: owner, DependsOnID: blocker, Type: "blocks"}, "")
			if len(plan.Edges[dest]) > before {
				plan.Restored++
			}
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
				parentDest, parentOpen, known := whereIs(row.ParentID, dest)
				switch {
				case known && parentDest == dest:
					addEdge(dest, d, "")
				case known:
					judge(d, "", dest, parentDest, rigRowOpen(*row), parentOpen, "its parent would be in another store, and one side is closed")
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

// sortedBeads returns rows sorted by id, without mutating the caller's slice.
func reverseHasID(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func sortedBeads(rows []beads.Bead) []beads.Bead {
	out := append([]beads.Bead(nil), rows...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
