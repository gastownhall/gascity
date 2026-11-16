package main

// The infra-class copy engine's batched halves (S5 §3.3, §3.4).
//
// The city migration used to pay one round trip per row and per edge at every
// stage: a payload read per edge, a transaction per created row and per added
// edge, a Get and two DepLists per verified row, and a commit per deleted row on
// Dolt. This file holds the store-free parts of the batched engine, written
// over the optional batch capabilities (beads.DependencyBatchLister,
// beads.DepMetadataBatchReader, beads.ForeignIDBatchCreator,
// beads.DepBatchWriter, beads.BatchDeleter) and over narrow read interfaces.
// The functions that hold the physical stores (infra_class_migrate.go,
// infra_class_clear.go) look the capabilities up with beads.As and pass them
// in; a store without one takes the per-row path.
//
// Every phase is cut into batches, and the boundary between two batches is
// where the engine may stop: a test seam fires there (infraEngineCrashPoint),
// and so does the operator's time budget, which pauses the run with
// *infraEnginePause. Nothing is decided inside a batch, and no transaction
// spans two.
//
// What makes stopping safe is the proof, which has not changed: the copy is
// proven by a bidirectional re-read of the source against the reopened
// binding before the marker, and the clear deletes only rows the manifest
// proves the binding holds. A paused or killed copy leaves stamped rows the
// next run keeps when they still equal the source (resumable copy) and
// deletes when they do not. A stale binding's re-copy runs under its old
// marker, so it also leaves its re-copy record, which keeps the partial
// binding stale until the re-copy's own marker lands.

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// The batch sizes. Variables only so a test can cut a small city into many
// batches; production never changes them.
var (
	// infraCopyRowBatch is the rows one CreateWithForeignIDBatch writes.
	infraCopyRowBatch = 500
	// infraCopyEdgeBatch is the edges one DepAddBatch writes.
	infraCopyEdgeBatch = 2000
	// infraReadBatch is the anchors one batched read asks about.
	infraReadBatch = 1000
	// infraClearChunk is the rows one DeleteBatch removes: native Dolt's
	// maximum, one commit each.
	infraClearChunk = 1000
	// infraDetachEdgeBatch is the edges one DepRemoveBatch removes. Native
	// Dolt removes an edge in about 55-65 ms on a loaded host (each removal
	// re-derives the blocked state it touched), so 250 take about 16 s, a
	// sevenfold margin under the 120 s operation budget; a molecule-shaped
	// chunk of 1,000 rows has about 2,000 edges and blew it in one call. The
	// extra commits cost about 65 ms each, under 1% of a sub-batch.
	infraDetachEdgeBatch = 250
)

// infraEngineCrashPoint is the test seam at every batch and phase boundary of
// the engine: point names the phase and how far it got. A non-nil error stops
// the engine there, as a kill would.
var infraEngineCrashPoint = func(string) error { return nil }

// infraEnginePause is the engine stopping at a boundary because the run's time
// budget is spent. Before the marker it leaves the city unconverged, or a stale
// binding's re-copy stale (infraRecopyRecordName); after it, the clear pending.
// The same command continues from where it stopped.
type infraEnginePause struct {
	Phase       string
	Done, Total int
}

func (p *infraEnginePause) Error() string {
	return fmt.Sprintf("paused in %s at %d/%d: the time budget is spent; run the same command again to continue", p.Phase, p.Done, p.Total)
}

// infraEngineClock is the run's time budget: a zero deadline never expires.
type infraEngineClock struct {
	deadline time.Time
}

func (c infraEngineClock) expired() bool {
	return !c.deadline.IsZero() && !time.Now().Before(c.deadline)
}

// infraEngineClockFor is the clock of the maintenance window open on
// cityPath, or one with no deadline (the boot gate's).
func infraEngineClockFor(cityPath string) infraEngineClock {
	if w := activeMigrationWindow(cityPath); w != nil {
		return infraEngineClock{deadline: w.deadline}
	}
	return infraEngineClock{}
}

// runInfraBatches runs fn over [0,total) in batches of size. The seam fires
// after each batch; the budget is checked after each batch too, so every run
// makes progress, and a run whose budget is spent stops at the next boundary.
func runInfraBatches(clock infraEngineClock, phase string, total, size int, fn func(start, end int) error) error {
	for start := 0; start < total; start += size {
		end := min(start+size, total)
		if err := fn(start, end); err != nil {
			return err
		}
		if err := infraEngineCrashPoint(fmt.Sprintf("%s %d/%d", phase, end, total)); err != nil {
			return err
		}
		if end < total && clock.expired() {
			return &infraEnginePause{Phase: phase, Done: end, Total: total}
		}
	}
	return nil
}

// infraRowLister is the row list every store answers.
type infraRowLister interface {
	List(query beads.ListQuery) ([]beads.Bead, error)
}

// infraEdgeLister is the per-anchor edge read every store answers.
type infraEdgeLister interface {
	DepList(id, direction string) ([]beads.Dep, error)
}

// readInfraDownEdges lists the down edges of ids: through batch when the store
// has the capability, otherwise one DepList per anchor. Every edge names its
// anchor as IssueID.
func readInfraDownEdges(batch beads.DependencyBatchLister, single infraEdgeLister, ids []string) (map[string][]beads.Dep, error) {
	out := make(map[string][]beads.Dep, len(ids))
	if batch != nil {
		for start := 0; start < len(ids); start += infraReadBatch {
			chunk := ids[start:min(start+infraReadBatch, len(ids))]
			got, err := batch.DepListBatch(chunk)
			if errors.Is(err, beads.ErrDepListBatchUnsupported) {
				batch = nil
				break
			}
			if err != nil {
				return nil, fmt.Errorf("listing the deps of %d bead(s): %w", len(chunk), err)
			}
			for id, deps := range got {
				out[id] = withInfraAnchor(id, deps)
			}
		}
		if batch != nil {
			return out, nil
		}
	}
	for _, id := range ids {
		deps, err := single.DepList(id, "down")
		if err != nil {
			return nil, fmt.Errorf("listing deps of %s: %w", id, err)
		}
		out[id] = withInfraAnchor(id, deps)
	}
	return out, nil
}

func withInfraAnchor(id string, deps []beads.Dep) []beads.Dep {
	out := make([]beads.Dep, 0, len(deps))
	for _, d := range deps {
		if d.IssueID == "" {
			d.IssueID = id
		}
		out = append(out, d)
	}
	return out
}

// readInfraPayloads reads the payload on every edge in edges (keyed by anchor)
// that wanted keeps: through batch when the store has it, otherwise one read
// per edge through single. A nil single that is still needed is a store that
// cannot be asked: refused with beads.ErrDepMetadataUnsupported, as the
// per-edge read refuses. Absent from the result means "carries nothing".
func readInfraPayloads(batch beads.DepMetadataBatchReader, single beads.DepMetadataReader, edges map[string][]beads.Dep, wanted func(beads.Dep) bool) (map[beads.DepPair]string, error) {
	anchors := make([]string, 0, len(edges))
	for id, deps := range edges {
		for _, d := range deps {
			if wanted(d) {
				anchors = append(anchors, id)
				break
			}
		}
	}
	sort.Strings(anchors)
	out := map[beads.DepPair]string{}
	if batch != nil {
		for start := 0; start < len(anchors); start += infraReadBatch {
			chunk := anchors[start:min(start+infraReadBatch, len(anchors))]
			got, err := batch.DepMetadataBatch(chunk)
			if errors.Is(err, beads.ErrDepMetadataUnsupported) && start == 0 {
				// A wrapper forwarding a capability its store lacks.
				batch = nil
				break
			}
			if err != nil {
				return nil, fmt.Errorf("reading the edge payloads of %d bead(s): %w", len(chunk), err)
			}
			for pair, payload := range got {
				out[pair] = payload
			}
		}
		if batch != nil {
			return out, nil
		}
	}
	for _, id := range anchors {
		for _, d := range edges[id] {
			if !wanted(d) {
				continue
			}
			if single == nil {
				return nil, fmt.Errorf("reading the payload on dep %s -> %s: %w", d.IssueID, d.DependsOnID, beads.ErrDepMetadataUnsupported)
			}
			payload, err := infraReadEdgePayloadFrom(single, d.IssueID, d.DependsOnID)
			if err != nil {
				return nil, err
			}
			if payload.Carried {
				out[beads.DepPair{IssueID: d.IssueID, DependsOnID: d.DependsOnID}] = payload.Payload
			}
		}
	}
	return out, nil
}

// infraCopyPlan is what one read of the source decides: the infrastructure
// slice, every edge the copy or the clear reads, and their payloads.
type infraCopyPlan struct {
	rows     []beads.Bead
	infraIDs map[string]bool
	// down is every down edge of each infra row.
	down map[string][]beads.Dep
	// up is, per infra row, its edges from rows outside the slice: the
	// dependents the clear backs up.
	up map[string][]beads.Dep
	// payloads holds the payload of every edge in down and up that carries
	// one; an edge absent here carries nothing.
	payloads map[beads.DepPair]string
	// carrying reports a within-infra edge with a payload.
	carrying bool
	// blind names the protocol of a payload-blind source (see
	// beads.DepMetadataAbsentByProtocol).
	blind string
	// boundary is the cross-boundary plan (D9, cross_boundary_plan.go): the
	// edges the copy writes, the gates it mints, and the boundary edges it
	// records on the moved rows. copied is boundary.Copy as a set.
	boundary crossBoundaryResult
	copied   map[beads.DepPair]bool
}

// payload is the payload the source holds on issue -> dependsOn.
func (p infraCopyPlan) payload(issue, dependsOn string) infraEdgePayload {
	if v, ok := p.payloads[beads.DepPair{IssueID: issue, DependsOnID: dependsOn}]; ok {
		return infraEdgePayload{Payload: v, Carried: true}
	}
	return infraEdgePayload{}
}

// copiedEdges is every edge the plan copies, each with the payload it
// carries, in a stable order: the edges between two moved rows, and an
// external: reference a moved row holds.
func (p infraCopyPlan) copiedEdges() []beads.DepWithPayload {
	out := make([]beads.DepWithPayload, 0, len(p.boundary.Copy))
	for _, d := range p.boundary.Copy {
		out = append(out, beads.DepWithPayload{Dep: d, Payload: p.payload(d.IssueID, d.DependsOnID).Payload})
	}
	return out
}

// plannedRow is the form of the source row b the copy creates in the
// binding: the stamp, and the record of its boundary edges not copied.
func (p infraCopyPlan) plannedRow(b beads.Bead) beads.Bead {
	return withInfraCrossEdgeRecords(infraMigrationRow(b), p.boundary.Records[b.ID])
}

// withInfraCrossEdgeRecords returns b carrying records as its
// gc.migrated_cross_edges, without mutating the caller's map; b unchanged
// when there are none.
func withInfraCrossEdgeRecords(b beads.Bead, records []rigCrossEdgeRecord) beads.Bead {
	if len(records) == 0 {
		return b
	}
	raw, err := json.Marshal(records)
	if err != nil {
		return b
	}
	metadata := make(beads.StringMap, len(b.Metadata)+1)
	for key, value := range b.Metadata {
		metadata[key] = value
	}
	metadata[beadmeta.MigratedCrossEdgesMetadataKey] = string(raw)
	b.Metadata = metadata
	return b
}

// infraCrossEdgesAtRisk is every edge from a work bead into a row of the
// plan, sorted: the clear drops each once the row is gone (it re-adds no
// edge, D9) and records it on its work bead.
func infraCrossEdgesAtRisk(plan infraCopyPlan) []infraBackupEdge {
	var out []infraBackupEdge
	for _, row := range plan.rows {
		for _, d := range plan.up[row.ID] {
			out = append(out, infraBackupEdge{IssueID: d.IssueID, DependsOnID: d.DependsOnID, Type: d.Type})
		}
	}
	sortInfraBackupEdges(out)
	return out
}

// infraPrepared is what prepareInfraDestination left in the binding for the
// copy to build on: the stamped rows it kept, and the edges between kept rows
// with the type and payload each already carries.
type infraPrepared struct {
	kept  map[string]bool
	edges map[beads.DepPair]beads.DepWithPayload
	// gates are the planned cross-store gates an earlier attempt already
	// minted, by (owner, blocker): kept, not minted again.
	gates map[beads.DepPair]string
	// stamped counts the rows an earlier attempt left in the binding, kept
	// or not.
	stamped int
}

// infraPayloadDifference compares the payloads on one bead's within-infra edges
// in both directions, from what each store reported (absent: carries
// nothing). It walks the union of the two edge sets, so a payload on an edge
// only the binding holds is a difference too; a copy that invented a payload
// is as much a changed graph as one that dropped it.
func infraPayloadDifference(id string, wantDeps, gotDeps []beads.Dep, infraIDs map[string]bool, want, got map[beads.DepPair]string) string {
	pairs := make(map[string]bool, len(wantDeps)+len(gotDeps))
	for _, d := range wantDeps {
		if infraIDs[d.DependsOnID] {
			pairs[d.DependsOnID] = true
		}
	}
	for _, d := range gotDeps {
		pairs[d.DependsOnID] = true
	}
	for _, dependsOn := range sortedMapKeys(pairs) {
		key := beads.DepPair{IssueID: id, DependsOnID: dependsOn}
		w, wOK := want[key]
		g, gOK := got[key]
		if w == g && wOK == gOK {
			continue
		}
		return fmt.Sprintf("dep %s -> %s carries %s in the binding, want %s", id, dependsOn,
			infraFormatEdgePayload(infraEdgePayload{Payload: g, Carried: gOK}), infraFormatEdgePayload(infraEdgePayload{Payload: w, Carried: wOK}))
	}
	return ""
}

// infraKeepStampedRow reports whether a row an earlier attempt stamped into the
// binding can stay: the source still holds it with every field the copy
// carries equal, and the binding holds no edge of it the source does not. A
// missing edge, or one whose type or payload changed, is written again by
// the copy (an upsert), so it needs no delete.
func infraKeepStampedRow(plan infraCopyPlan, want beads.Bead, got beads.Bead, gotDeps []beads.Dep, gates map[beads.DepPair]string) bool {
	if beadCopyDifference(plan.plannedRow(want), got) != "" || infraCopyClassDifference(want, got) != "" {
		return false
	}
	planned := map[string]bool{}
	for _, g := range plan.boundary.Gates {
		if id := gates[beads.DepPair{IssueID: g.Owner.ID, DependsOnID: g.Blocker}]; id != "" && g.Owner.ID == want.ID {
			planned[id] = true
		}
	}
	for _, d := range plan.down[want.ID] {
		if plan.copied[beads.DepPair{IssueID: want.ID, DependsOnID: d.DependsOnID}] {
			planned[d.DependsOnID] = true
		}
	}
	for _, d := range gotDeps {
		if !planned[d.DependsOnID] {
			return false
		}
	}
	return true
}

// infraBackupEdgeFilter keeps the edges the clear backs up for rows: each
// row's own edges down, and the edges onto a row from a row that is not
// retained.
func infraBackupEdgeFilter(rows []beads.Bead, retained map[string]bool) func(beads.Dep) bool {
	backed := make(map[string]bool, len(rows))
	for _, b := range rows {
		backed[b.ID] = true
	}
	return func(d beads.Dep) bool {
		return backed[d.IssueID] || (backed[d.DependsOnID] && !retained[d.IssueID])
	}
}

// infraBackupEntriesFrom builds the backup entries infraBackupEntryFor reads
// row by row, from every row's down edges (down, keyed by anchor) and the
// payloads read for them: a row's Deps are its edges down, its Dependents the
// edges onto it from a row that is not retained, both sorted.
func infraBackupEntriesFrom(rows []beads.Bead, down map[string][]beads.Dep, payloads map[beads.DepPair]string, retained map[string]bool) []infraBackupEntry {
	edge := func(d beads.Dep) infraBackupEdge {
		payload, carried := payloads[beads.DepPair{IssueID: d.IssueID, DependsOnID: d.DependsOnID}]
		return infraBackupEdge{IssueID: d.IssueID, DependsOnID: d.DependsOnID, Type: d.Type, Payload: payload, Carried: carried}
	}
	backed := make(map[string]bool, len(rows))
	for _, b := range rows {
		backed[b.ID] = true
	}
	dependents := map[string][]infraBackupEdge{}
	for _, deps := range down {
		for _, d := range deps {
			if backed[d.DependsOnID] && !retained[d.IssueID] {
				dependents[d.DependsOnID] = append(dependents[d.DependsOnID], edge(d))
			}
		}
	}
	out := make([]infraBackupEntry, 0, len(rows))
	for _, b := range rows {
		entry := infraBackupEntry{Bead: b, Class: coordclass.Classify(b).String(), Dependents: dependents[b.ID]}
		for _, d := range down[b.ID] {
			entry.Deps = append(entry.Deps, edge(d))
		}
		sortInfraBackupEdges(entry.Deps)
		sortInfraBackupEdges(entry.Dependents)
		out = append(out, entry)
	}
	return out
}
