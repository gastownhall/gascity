package main

// Clearing the retained source: the step that ends co-residency.
//
// `gc storage migrate` copies every infrastructure bead into the binding with
// its id preserved. Until this step existed it then KEPT the work-store row
// verbatim, so every relocated bead lived twice under one id. After the marker
// the binding is the only authoritative copy — every write since cutover lands
// there — but the retained row stayed a live candidate on every read that
// reaches the work store first: the demand and claim federations (work leg
// first, first leg wins), `gc hook`'s per-store reads, raw `bd`, the reaper.
// A formula step closed in the binding therefore stayed open on the surface
// that dispatches it, and was re-dispatched forever (#5987).
//
// The fix is a data-model fix rather than one more read path taught to prefer
// the binding: after a proven copy the work store holds no infrastructure bead
// at all, which is exactly the invariant a city born split already serves
// under. One id, one row — so no reader, including the ones gc cannot
// intercept, can be served the frozen copy.
//
// # What replaces the retained source
//
// The retained rows had value as a record of the pre-cutover state: the
// re-converge recipe re-copied from them, and payload repair (ga-67pm3) reads
// them. That value moves into a backup written beside the manifest
// (infra.retained-source.jsonl) BEFORE anything is deleted: every row as
// beads.Bead models it — the same fields the copy carried and proved — and
// every edge touching it with its payload. Fields only bd models (notes,
// comments, close reason, event history) are not in it; on a native-Dolt work
// store the Dolt history of each removal is their backstop. The backup is re-read
// from disk and proven equal to the source rows it replaces, and only then is
// a single row removed. A stale binding (marker present, database gone) is
// re-copied from the backup (`gc storage migrate --from-backup`), never from a
// work store that no longer holds the slice.
//
// # Classification
//
// Every infrastructure row in the work store is classified against the copy
// manifest, the record of what the equality stage proved the binding received:
//
//   - In the manifest: a retained copy. Cleared, whether or not the binding
//     still holds it — a row the binding's own GC collected is a zombie the
//     demand surface would otherwise serve forever.
//   - Not in the manifest: a write the proof never covered (a strand, or the
//     residue an interrupted recovery left). Nothing proves the binding holds
//     it, so the clear refuses and names `gc storage recover-stranded`, which
//     proves it in and extends the manifest. The clear then removes it.
//
// # Edges that cross into work
//
// A work bead can depend on a relocated bead (a v1 `cook --attach` graft is
// the in-tree producer). No cross-store edge is ever written (D9), so the
// clear never re-adds one: each edge from a row that stays onto a row the
// clear removes is removed with it and recorded in the backup. The shared
// cross-boundary plan (cross_boundary_plan.go) decides first, against the
// binding's copy of the removed row, which edges still matter, and refuses
// the clear before any row goes while one does: an open work bead blocked by
// a row the binding has not satisfied, or a membership edge (tracks,
// parent-child) with both ends open. Finishing the row, or removing the edge
// with `gc bd dep remove`, lets the clear run. A copy by this build has
// already refused those shapes; the clear's check is what holds a city an
// earlier build converged without them (retained copies, S51).
//
// Cities an earlier build cleared keep the cross edges that clear re-added;
// the doctor's dangling-blocking-edges check and the cross-store read rule
// cover them, and nothing here touches them.
//
// # Resumability
//
// Nothing here is a single transaction across stores, so every step is
// idempotent instead. The backup is a union keyed by id — a rerun keeps the
// entries of rows an earlier run already removed and refreshes the rest — and
// it is proven before each delete pass. Rows already gone are skipped. A run
// interrupted anywhere leaves either nothing removed or a city the next boot
// refuses with the command that finishes the job.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// infraRetainedBackupName is the backup of the work-store rows the clear
// removed, written at the binding root beside the marker and the manifest.
const infraRetainedBackupName = "infra.retained-source.jsonl"

// infraRetainedBackupFormat versions the backup's line format. A reader refuses
// any other value rather than guess at a shape it was not written for.
const infraRetainedBackupFormat = "gc.infra.retained-source/v1"

// RetainedBackupPath is where the clear records the rows it removed.
func (t infraBindingTarget) RetainedBackupPath() string {
	return filepath.Join(t.Root, infraRetainedBackupName)
}

// infraBackupEdge is one dependency edge as the backup records it: the pair,
// the type, and the payload the source carried on it, absence kept distinct
// from an empty payload.
type infraBackupEdge struct {
	IssueID     string `json:"issue_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
	Payload     string `json:"payload,omitempty"`
	Carried     bool   `json:"carried,omitempty"`
	// Disposition is what the migration did with the edge: copied, gated,
	// copied-external or dropped. The rig import sets it from its plan
	// (rig_import.go), the city clear from its session, the manifest and the
	// binding's record (withInfraClearDispositions). A backup written by an
	// earlier build leaves it empty.
	Disposition string `json:"disposition,omitempty"`
}

func (e infraBackupEdge) dep() beads.Dep {
	return beads.Dep{IssueID: e.IssueID, DependsOnID: e.DependsOnID, Type: e.Type}
}

func (e infraBackupEdge) String() string {
	return fmt.Sprintf("%s -[%s]-> %s", e.IssueID, e.Type, e.DependsOnID)
}

// infraBackupHeader is the backup's first line.
type infraBackupHeader struct {
	Format    string    `json:"format"`
	Binding   string    `json:"binding"`
	Beads     int       `json:"beads"`
	WrittenAt time.Time `json:"written_at"`
}

// infraBackupEntry is one removed row with every edge touching it: its own
// outbound edges, and the inbound edges from beads the clear keeps (work beads
// depending on it). Inbound edges from other cleared rows are recorded once,
// as those rows' outbound edges.
type infraBackupEntry struct {
	Bead       beads.Bead        `json:"bead"`
	Class      string            `json:"class"`
	Deps       []infraBackupEdge `json:"deps,omitempty"`
	Dependents []infraBackupEdge `json:"dependents,omitempty"`
	// Endpoints is which bead each far end of the row's edges was when a purge
	// backed the row up, by id and created_at, for every far end the work store
	// held that is not itself a backed-up row (infraPurgeBackupEntries). The
	// clear and recover-stranded record none.
	Endpoints []infraPurgeRowKey `json:"endpoints,omitempty"`
}

// infraClearResult is what one clear pass did.
type infraClearResult struct {
	// Cleared counts the work-store rows this pass removed.
	Cleared int
	// Backup is the path of the backup holding every row ever cleared from
	// this city, or "" when there has never been anything to clear.
	Backup string
	// Dropped are the edges from rows that stay onto rows this session
	// removed: never re-added (D9), each recorded in the backup.
	Dropped []infraBackupEdge
	// LostCrossEdges is every cross-store edge a clear by an earlier build
	// could not keep, as its cleared note records. A clear that never
	// re-adds an edge loses none, so this build adds nothing to it.
	LostCrossEdges []string
	// Recorded counts this run's dropped edges recorded on their work beads
	// (gc.migrated_cross_edges).
	Recorded int
}

// infraUnprovenSourceRows is the clear's refusal for work-store infrastructure
// rows the proven copy never carried. Removing them would delete the only copy
// anything proved exists.
type infraUnprovenSourceRows struct {
	IDs []string
}

func (e *infraUnprovenSourceRows) Error() string {
	return fmt.Sprintf("the work store holds %d infrastructure bead(s) the proven copy never carried (%s), so nothing proves the binding holds them and they cannot be cleared. Copy them in with `%s` first (it asks per id, --%s or --%s, about any whose id the binding holds for a different bead), then run this again",
		len(e.IDs), infraStrandedIDList(e.IDs), storageRecoveryInstruction(), strandedReplaceBindingRowFlag, strandedDropWorkRowFlag)
}

// infraUnboundedLegacyRows is the clear's refusal for work-store rows the
// manifest lists with no time that tells the copied bead from a later one that
// reused its id: a legacy entry with no trustworthy bound, or a row that
// carries no created_at for an entry to be judged against.
type infraUnboundedLegacyRows struct {
	IDs      []string
	Manifest string
}

func (e *infraUnboundedLegacyRows) Error() string {
	return fmt.Sprintf("the copy manifest %s lists %d work-store infrastructure bead(s) (%s) with no time that tells the bead it copied from a later one that reused the id: the manifest records none, or the row carries no created_at. They cannot be cleared on its word. `%s` resolves them (it records the ones the binding still holds once it proves them the same bead, moves closed ones the binding no longer holds, and asks per id about open ones: --move or --record-carried); run it, then run this again",
		e.Manifest, len(e.IDs), infraStrandedIDList(e.IDs), storageRecoveryInstruction())
}

// Test seams for the clear's fault-injection rows.
var (
	// infraClearBackupWritten runs after the backup is renamed into place and
	// before it is re-read, so a test can corrupt what the proof reads.
	infraClearBackupWritten = func(string) {}
	// infraClearBeforeDelete runs before each row is removed.
	infraClearBeforeDelete = func(string) error { return nil }
)

// clearRetainedInfraCopies removes, from the city work store, every
// infrastructure row the proven copy delivered to the binding — after backing
// each one up and proving the backup. It is run only past the convergence
// marker, by the operator command, with the fleet stopped.
//
// A clear is a SESSION that may span several runs: a run killed mid-delete
// leaves the cleared note open, naming every id the session has taken on, and
// the next run finishes it. The cross-store edge plan is built from the backup
// entries of the whole session — not from the rows this run still sees —
// because a row an earlier run already removed is exactly the one whose
// dependents still need releasing or restoring.
//
// announce receives the operator-facing census of cross-store edges before any
// row is removed, so the edges a backend will drop are on record first.
func clearRetainedInfraCopies(cityPath string, target infraBindingTarget, announce io.Writer) (infraClearResult, error) {
	result := infraClearResult{}
	manifest, recorded, err := readInfraCopyManifestRecord(target)
	if err != nil {
		return result, err
	}
	if !recorded {
		return result, fmt.Errorf("%s converged before %s was recorded, so nothing says which work-store rows the copy delivered and none can be cleared. Re-converge the binding to record the manifest", target.Database, target.ManifestPath())
	}
	note, notePresent, err := readInfraClearedNote(cityPath)
	if err != nil {
		return result, err
	}
	resuming := notePresent && !note.Complete

	source, err := openInfraMigrationSource(cityPath)
	if err != nil {
		return result, fmt.Errorf("opening work store: %w", err)
	}
	defer closeBeadStoreHandle(source) //nolint:errcheck // best-effort close

	rows, all, err := readInfraSnapshotRows(source, manifest.Generation)
	if err != nil {
		return result, err
	}
	allIDs := infraRowIDs(all)
	if present, err := infraPathExists(target.RetainedBackupPath()); err != nil {
		return result, fmt.Errorf("reading the retained-source backup: %w", err)
	} else if present {
		result.Backup = target.RetainedBackupPath()
	}
	if len(rows) == 0 && !resuming {
		return result, nil
	}

	var unproven, unbounded, retained []beads.Bead
	// Carried, by (id, created_at): a row that merely reuses a manifest id was
	// never copied, and deleting it here would destroy the only copy. Nor is a
	// row carried when a bare id only presumes it reused, when its legacy entry
	// has no trustworthy bound, or when it has no created_at to judge: nothing
	// says it is the bead the copy carried, so it is not deleted on a guess.
	for _, b := range rows {
		switch manifest.Match(b) {
		case infraManifestCarried:
			retained = append(retained, b)
		case infraManifestLegacyUnbounded:
			unbounded = append(unbounded, b)
		default:
			unproven = append(unproven, b)
		}
	}
	if len(unbounded) > 0 && len(unproven) == 0 {
		ids := make([]string, 0, len(unbounded))
		for _, b := range unbounded {
			ids = append(ids, b.ID)
		}
		sort.Strings(ids)
		return result, &infraUnboundedLegacyRows{IDs: ids, Manifest: target.ManifestPath()}
	}
	unproven = append(unproven, unbounded...)
	if len(unproven) > 0 {
		ids := make([]string, 0, len(unproven))
		for _, b := range unproven {
			ids = append(ids, b.ID)
		}
		sort.Strings(ids)
		return result, &infraUnprovenSourceRows{IDs: ids}
	}

	held, err := infraBindingRows(target)
	if err != nil {
		return result, err
	}

	retainedIDs := make(map[string]bool, len(retained))
	for _, b := range retained {
		retainedIDs[b.ID] = true
	}
	// The session: every id an unfinished earlier run took on, plus this run's.
	session := map[string]bool{}
	if resuming {
		for _, id := range note.Pending {
			session[id] = true
		}
	}
	for id := range retainedIDs {
		session[id] = true
	}

	// Backup first, proven from disk, before any row is touched.
	previous, _, err := readInfraRetainedBackup(target)
	if err != nil {
		return result, err
	}
	// Within an open session the FIRST entry for an id wins: it was proven
	// before any row of the session was touched. A row an interrupted run
	// left behind has since lost edges — its own outbound deps are removed
	// before its delete, and a cascading store drops inbound edges with every
	// earlier delete — so re-reading it now would overwrite the only full
	// record of its topology with a stripped one.
	sessionBackup := map[string]infraBackupEntry{}
	if resuming {
		for _, e := range previous {
			if session[e.Bead.ID] {
				sessionBackup[e.Bead.ID] = e
			}
		}
	}
	// Each row's edges and their payloads: in batches over every row the work
	// store holds when it lists edges so (a row's dependents are the down
	// edges of whatever holds them), otherwise both directions row by row.
	var fresh []beads.Bead
	for _, b := range retained {
		if _, ok := sessionBackup[b.ID]; !ok {
			fresh = append(fresh, b)
		}
	}
	read := make(map[string]infraBackupEntry, len(fresh))
	if batch, ok := beads.As[beads.DependencyBatchLister](source); ok && len(fresh) > 0 {
		down, err := readInfraDownEdges(batch, source, sortedMapKeys(allIDs))
		if err != nil {
			return result, err
		}
		payloads := map[beads.DepPair]string{}
		if _, blind := beads.DepMetadataAbsent(source); !blind {
			reader, _ := beads.As[beads.DepMetadataReader](source)
			batchReader, _ := beads.As[beads.DepMetadataBatchReader](source)
			wanted := infraBackupEdgeFilter(fresh, retainedIDs)
			if payloads, err = readInfraPayloads(batchReader, reader, down, wanted); err != nil {
				return result, err
			}
		}
		for _, entry := range infraBackupEntriesFrom(fresh, down, payloads, retainedIDs) {
			read[entry.Bead.ID] = entry
		}
	} else {
		for _, b := range fresh {
			entry, err := infraBackupEntryFor(source, b, retainedIDs)
			if err != nil {
				return result, err
			}
			read[b.ID] = entry
		}
	}
	entries := make([]infraBackupEntry, 0, len(retained))
	for _, b := range retained {
		if prior, ok := sessionBackup[b.ID]; ok {
			entries = append(entries, prior)
			continue
		}
		entries = append(entries, withInfraClearDispositions(read[b.ID], session, manifest.IDs, held))
	}
	merged := mergeInfraBackupEntries(previous, entries)
	if len(entries) > 0 {
		if err := writeInfraRetainedBackup(target, merged); err != nil {
			return result, err
		}
		infraClearBackupWritten(target.RetainedBackupPath())
		if err := verifyInfraRetainedBackup(target, entries); err != nil {
			return result, err
		}
		result.Backup = target.RetainedBackupPath()
	}

	// The boundary edges (D9), over the whole session, decided by the shared
	// plan against the binding copy of each removed row: an edge from a row
	// that stays onto one this clear removes is never re-added, so one the
	// plan refuses (an open blocker the binding has not satisfied, a live
	// membership edge) stops the clear before any row goes, and the rest are
	// dropped, as their backup entries record. A run interrupted anywhere
	// before the delete checks again (infraClearBoundaryRefusals reads the
	// edges still in the work store).
	refusals, dropped, err := infraClearBoundaryRefusals(source, merged, session, all, held, manifest.Generation)
	if err != nil {
		return result, err
	}
	if len(refusals) > 0 {
		return result, &infraClearBoundaryRefusal{Refusals: refusals}
	}
	announceInfraCrossEdges(announce, dropped)
	if err := infraEngineCrashPoint("clear checked"); err != nil {
		return result, err
	}

	// The city-side record, before the first row goes: from here on the work
	// store can no longer stand in for the binding, a binding root that later
	// reads as empty must not be mistaken for a city with nothing to move, and
	// a run killed from here on is resumed from the session it names.
	//
	// Pending is written once, whole, and does not shrink as chunks go. A
	// resumed run takes its session from Pending, and the session is what
	// the boundary check above and the detach below are decided over: it
	// tells a removed row's leftover internal edges from an edge its backup
	// does not record.
	pending := sortedMapKeys(session)
	note = infraClearedNote{Binding: target.Binding, Database: target.Database, Backup: target.RetainedBackupPath(), Pending: pending, LostCrossEdges: note.LostCrossEdges}
	if err := writeInfraClearedNote(cityPath, note); err != nil {
		return result, err
	}

	// The rows go in chunks, and every row is detached before it goes: each
	// edge touching it is removed and the remainder proven empty
	// (detachInfraClearedRow), so a native-Dolt or bd delete has no neighbor
	// whose text it would rewrite (#8786). A store that deletes in batches
	// has the whole chunk detached and then removed in one call, one commit
	// on Dolt; any other has each row detached and deleted in turn. The budget is checked between chunks; a paused
	// clear leaves the note pending, and the same command finishes it from
	// the session the note names. A wrapper that forwards DeleteBatch over a
	// store without it reports beads.ErrBatchDeleteUnsupported, and the rows
	// then go one by one.
	deleter, canBatch := beads.As[beads.BatchDeleter](source)
	err = runInfraBatches(infraEngineClockFor(cityPath), "clearing", len(entries), infraClearChunk, func(start, end int) error {
		chunk := entries[start:end]
		if !canBatch {
			for i, entry := range chunk {
				if err := infraClearBeforeDelete(entry.Bead.ID); err != nil {
					return err
				}
				if err := detachInfraClearedRow(source, chunk[i:i+1], session, allIDs); err != nil {
					return err
				}
				if err := source.Delete(entry.Bead.ID); err != nil && !errors.Is(err, beads.ErrNotFound) {
					return fmt.Errorf("removing the retained copy %s from the work store: %w", entry.Bead.ID, err)
				}
				result.Cleared++
			}
			return nil
		}
		ids := make([]string, 0, len(chunk))
		for _, entry := range chunk {
			if err := infraClearBeforeDelete(entry.Bead.ID); err != nil {
				return err
			}
			ids = append(ids, entry.Bead.ID)
		}
		if err := detachInfraClearedRow(source, chunk, session, allIDs); err != nil {
			return err
		}
		err := deleter.DeleteBatch(ids)
		if errors.Is(err, beads.ErrBatchDeleteUnsupported) {
			canBatch = false
			for _, id := range ids {
				if err := source.Delete(id); err != nil && !errors.Is(err, beads.ErrNotFound) {
					return fmt.Errorf("removing the retained copy %s from the work store: %w", id, err)
				}
				result.Cleared++
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("removing %d retained cop(ies) from the work store: %w", len(ids), err)
		}
		result.Cleared += len(ids)
		return nil
	})
	if err != nil {
		return result, err
	}
	result.Dropped = dropped
	// Each dropped edge is recorded on its work bead, in the rig importer's
	// shape, so the bead keeps its provenance (ga-4tnypl). One metadata write
	// per work bead; a resumed run adds nothing twice.
	movedStatus := make(map[string]string, len(merged))
	for _, entry := range merged {
		movedStatus[entry.Bead.ID] = entry.Bead.Status
	}
	byWork := map[string][]infraBackupEdge{}
	for _, edge := range dropped {
		byWork[edge.IssueID] = append(byWork[edge.IssueID], edge)
	}
	for _, workID := range sortedMapKeys(byWork) {
		work, err := source.Get(workID)
		if errors.Is(err, beads.ErrNotFound) {
			continue
		}
		if err != nil {
			return result, fmt.Errorf("reading %s to record the cross-store edges the clear dropped: %w", workID, err)
		}
		value, changed, err := mergeCrossEdgeRecords(work.Metadata[beadmeta.MigratedCrossEdgesMetadataKey], infraDroppedCrossEdgeRecords(byWork[workID], movedStatus))
		if err != nil {
			return result, fmt.Errorf("recording the cross-store edges the clear dropped on %s: %w", workID, err)
		}
		if changed {
			if err := source.SetMetadata(workID, beadmeta.MigratedCrossEdgesMetadataKey, value); err != nil {
				return result, fmt.Errorf("recording the cross-store edges the clear dropped on %s: %w", workID, err)
			}
		}
		result.Recorded += len(byWork[workID])
	}

	after, err := readInfraSnapshotAt(source, manifest.Generation)
	if err != nil {
		return result, fmt.Errorf("re-reading the work store after the clear: %w", err)
	}
	var left []string
	for _, b := range after {
		if session[b.ID] {
			left = append(left, b.ID)
		}
	}
	if len(left) > 0 {
		sort.Strings(left)
		return result, fmt.Errorf("the work store still holds %d retained cop(ies) after the clear: %s", len(left), infraStrandedIDList(left))
	}

	note.Complete = true
	note.Pending = nil
	if err := writeInfraClearedNote(cityPath, note); err != nil {
		return result, err
	}
	result.LostCrossEdges = note.LostCrossEdges
	return result, nil
}

// detachInfraClearedRow removes every edge touching a chunk of rows before
// they are deleted, so the delete has no neighbor to follow. A native-Dolt or
// bd work store's delete rewrites each linked neighbor's mention of the
// deleted id to "[deleted:<id>]" (ga-w4p3pw); the rows are moved, not deleted,
// and a work bead that cites one must keep its text. Each row's own edges and
// its dependents outside the session are in its backup entry; a dependent
// inside the session is a row this clear also removes, whose edge its own
// entry records. No edge is re-added afterwards (D9). An edge the backup does
// not record refuses the
// delete rather than lose it. listed is every row the work store's list
// returned, which the batched backup read the dependents from.
//
// The edges go in beads.DepBatchRemover calls of infraDetachEdgeBatch, one
// commit each on Dolt, where the store has one: DepRemove commits once per
// edge. On native Dolt that
// halves the cost (measured on a loaded host: about 130 ms an edge one by
// one, 60 ms in one transaction, where the library re-derives the blocked
// state the edge touched). The remainder is then read row by row, and the
// session-internal edges it finds go in further calls.
func detachInfraClearedRow(source beads.Store, chunk []infraBackupEntry, session, listed map[string]bool) error {
	batch, canBatch := beads.As[beads.DepBatchRemover](source)
	remove := func(pairs []beads.DepPair) error {
		for start := 0; start < len(pairs); start += infraDetachEdgeBatch {
			sub := pairs[start:min(start+infraDetachEdgeBatch, len(pairs))]
			if canBatch {
				err := batch.DepRemoveBatch(sub)
				if !errors.Is(err, beads.ErrDepRemoveBatchUnsupported) {
					if err != nil {
						return fmt.Errorf("removing %d dep(s) of the retained copies from the work store: %w", len(sub), err)
					}
					continue
				}
				canBatch = false
			}
			for _, p := range sub {
				if err := source.DepRemove(p.IssueID, p.DependsOnID); err != nil && !errors.Is(err, beads.ErrNotFound) {
					return fmt.Errorf("removing dep %s -> %s from the work store: %w", p.IssueID, p.DependsOnID, err)
				}
			}
		}
		return nil
	}
	var recorded []beads.DepPair
	for _, entry := range chunk {
		for _, edges := range [][]infraBackupEdge{entry.Deps, entry.Dependents} {
			for _, edge := range edges {
				recorded = append(recorded, beads.DepPair{IssueID: edge.IssueID, DependsOnID: edge.DependsOnID})
			}
		}
	}
	if err := remove(recorded); err != nil {
		return err
	}
	var internal []beads.DepPair
	for _, entry := range chunk {
		id := entry.Bead.ID
		for _, dir := range []string{"down", "up"} {
			live, err := source.DepList(id, dir)
			if err != nil {
				return fmt.Errorf("listing the remaining edges of %s: %w", id, err)
			}
			for _, d := range live {
				if dir == "up" && session[d.IssueID] {
					internal = append(internal, beads.DepPair{IssueID: d.IssueID, DependsOnID: d.DependsOnID})
					continue
				}
				if dir == "up" && !listed[d.IssueID] {
					return fmt.Errorf("removing the retained copy %s from the work store: %s depends on it -[%s]->, and the work store's list does not return %s (a row whose metadata does not parse is left out of the list), so the backup could not record the edge; deleting %s would drop it and rewrite the text of %s. Repair or remove %s, then run the command again. The row stays in the work store", id, d.IssueID, d.Type, d.IssueID, id, d.IssueID, d.IssueID)
				}
				return fmt.Errorf("removing the retained copy %s from the work store: it still has the edge %s -[%s]-> %s, which its backup entry does not record; deleting it would drop the edge and rewrite the text of %s. The row stays in the work store", id, d.IssueID, d.Type, d.DependsOnID, d.IssueID)
			}
		}
	}
	return remove(internal)
}

// announceInfraCrossEdges prints, before any row is removed, every edge from a
// work bead into a bead this clear removes: each is dropped, not re-added.
func announceInfraCrossEdges(w io.Writer, dropped []infraBackupEdge) {
	if w == nil || len(dropped) == 0 {
		return
	}
	fmt.Fprintf(w, "cross-store edges dropped (a work store never holds an edge onto a bead it no longer has; each is recorded in the backup): %d\n", len(dropped)) //nolint:errcheck // best-effort output
	for _, e := range dropped {
		fmt.Fprintf(w, "  %s\n", e) //nolint:errcheck // best-effort output
	}
}

// infraClearBoundaryRefusal is the clear's refusal of edges from rows that
// stay onto rows it would remove, by bead, each with its fix. Nothing was
// removed.
type infraClearBoundaryRefusal struct {
	Refusals []rigImportRefusal
}

func (e *infraClearBoundaryRefusal) Error() string {
	lines := make([]string, 0, len(e.Refusals))
	for _, r := range e.Refusals {
		lines = append(lines, "  - "+r.String())
	}
	return fmt.Sprintf("%d edge(s) from a work bead onto a bead the clear would remove still matter, and the clear never re-adds an edge; nothing was removed:\n%s", len(e.Refusals), strings.Join(lines, "\n"))
}

// infraClearBoundaryRefusals runs the shared cross-boundary plan
// (cross_boundary_plan.go) over the session's edges from rows that stay onto
// rows the clear removes, each removed row judged by its binding copy (the
// binding is authoritative past the marker; a row the binding's GC collected
// reads as closed). It returns the refusals for edges the work store still
// holds, and every such edge of the session as dropped.
//
// A refusal is checked against the live work store, not only the backup: an
// edge an interrupted earlier run already detached (its row then deleted, or
// about to be) was judged before it went, and judging it again could wedge a
// pending clear on a blocker that has since reopened.
func infraClearBoundaryRefusals(source infraEdgeLister, entries []infraBackupEntry, session map[string]bool, all, held map[string]beads.Bead, gen coordclass.Generation) ([]rigImportRefusal, []infraBackupEdge, error) {
	snap := rigSnapshot{byID: map[string]beads.Bead{}, down: map[string][]beads.Dep{}}
	moved := map[string]bool{}
	var dropped []infraBackupEdge
	for _, entry := range entries {
		if !session[entry.Bead.ID] {
			continue
		}
		d := entry.Bead
		if bound, ok := held[d.ID]; ok {
			d.Status, d.Metadata = bound.Status, bound.Metadata
		} else {
			d.Status, d.Metadata = "closed", beads.StringMap{}
		}
		snap.byID[d.ID], moved[d.ID] = d, true
		for _, edge := range entry.Dependents {
			if session[edge.IssueID] {
				continue
			}
			dropped = append(dropped, edge)
			if w, ok := all[edge.IssueID]; ok {
				snap.byID[w.ID] = w
			}
			snap.down[edge.IssueID] = append(snap.down[edge.IssueID], edge.dep())
		}
	}
	for _, id := range sortedMapKeys(snap.byID) {
		snap.rows = append(snap.rows, snap.byID[id])
	}
	// A removed row is infrastructure whatever its binding copy now carries:
	// the manifest's classifier selected it.
	moves := infraClassMoves(gen)
	plan := crossBoundaryPlan(crossBoundaryInput{
		Snap:       snap,
		Moves:      func(b beads.Bead) bool { return moved[b.ID] || moves(b) },
		Moved:      moved,
		WorkNoun:   "work",
		SourceNoun: "the work store",
	})
	var refusals []rigImportRefusal
	for _, r := range plan.Refusals {
		live, err := infraEdgeLive(source, r.IDs[0], r.IDs[1])
		if err != nil {
			return nil, nil, err
		}
		if live {
			refusals = append(refusals, r)
		}
	}
	sortInfraBackupEdges(dropped)
	return refusals, dropped, nil
}

// infraEdgeLive reports whether the work store still holds the edge from ->
// to.
func infraEdgeLive(source infraEdgeLister, from, to string) (bool, error) {
	deps, err := source.DepList(from, "down")
	if err != nil {
		return false, fmt.Errorf("listing the remaining edges of %s: %w", from, err)
	}
	for _, d := range deps {
		if d.DependsOnID == to {
			return true, nil
		}
	}
	return false, nil
}

// withInfraClearDispositions marks each edge of a fresh backup entry with what
// the city migration did with it (infraBackupEdge.Disposition): an edge onto
// a row the copy carried (this session's, or one an earlier session already
// removed) was copied; an edge onto the row from a row that stays is dropped
// by this clear; any other edge of the row is what its binding copy's
// gc.migrated_cross_edges record says (gated, copied-external, dropped),
// dropped when it says nothing.
func withInfraClearDispositions(entry infraBackupEntry, session, carried map[string]bool, held map[string]beads.Bead) infraBackupEntry {
	recorded := map[string]string{}
	if raw := strings.TrimSpace(held[entry.Bead.ID].Metadata[beadmeta.MigratedCrossEdgesMetadataKey]); raw != "" {
		var recs []rigCrossEdgeRecord
		if json.Unmarshal([]byte(raw), &recs) == nil {
			for _, r := range recs {
				if r.Dir == "out" {
					recorded[r.Other] = r.Action
				}
			}
		}
	}
	deps := make([]infraBackupEdge, 0, len(entry.Deps))
	for _, e := range entry.Deps {
		switch {
		case session[e.DependsOnID] || carried[e.DependsOnID]:
			e.Disposition = rigEdgeCopied
		case recorded[e.DependsOnID] != "":
			e.Disposition = recorded[e.DependsOnID]
		default:
			e.Disposition = rigEdgeDropped
		}
		deps = append(deps, e)
	}
	dependents := make([]infraBackupEdge, 0, len(entry.Dependents))
	for _, e := range entry.Dependents {
		e.Disposition = rigEdgeDropped
		dependents = append(dependents, e)
	}
	if len(entry.Deps) > 0 {
		entry.Deps = deps
	}
	if len(entry.Dependents) > 0 {
		entry.Dependents = dependents
	}
	return entry
}

// infraBindingRows lists what the binding holds, keyed by id, read-only.
func infraBindingRows(target infraBindingTarget) (map[string]beads.Bead, error) {
	binding, err := openInfraBindingReadOnly(target)
	if err != nil {
		return nil, fmt.Errorf("opening binding %q at %s: %w", target.Binding, target.Database, err)
	}
	defer closeBeadStoreHandle(binding) //nolint:errcheck // best-effort close
	list, err := binding.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return nil, fmt.Errorf("listing binding: %w", err)
	}
	held := make(map[string]beads.Bead, len(list))
	for _, b := range list {
		held[b.ID] = b
	}
	return held, nil
}

// infraBackupEntryFor reads one retained row's edges, with payloads.
func infraBackupEntryFor(source beads.Store, b beads.Bead, retained map[string]bool) (infraBackupEntry, error) {
	entry := infraBackupEntry{Bead: b, Class: coordclass.Classify(b).String()}
	down, err := source.DepList(b.ID, "down")
	if err != nil {
		return entry, fmt.Errorf("listing deps of %s: %w", b.ID, err)
	}
	for _, d := range down {
		edge, err := infraBackupEdgeFor(source, d)
		if err != nil {
			return entry, err
		}
		entry.Deps = append(entry.Deps, edge)
	}
	up, err := source.DepList(b.ID, "up")
	if err != nil {
		return entry, fmt.Errorf("listing dependents of %s: %w", b.ID, err)
	}
	for _, d := range up {
		if retained[d.IssueID] {
			continue
		}
		edge, err := infraBackupEdgeFor(source, d)
		if err != nil {
			return entry, err
		}
		entry.Dependents = append(entry.Dependents, edge)
	}
	sortInfraBackupEdges(entry.Deps)
	sortInfraBackupEdges(entry.Dependents)
	return entry, nil
}

func infraBackupEdgeFor(source beads.Store, d beads.Dep) (infraBackupEdge, error) {
	payload, err := infraReadEdgePayload(source, d.IssueID, d.DependsOnID)
	if err != nil {
		return infraBackupEdge{}, err
	}
	return infraBackupEdge{IssueID: d.IssueID, DependsOnID: d.DependsOnID, Type: d.Type, Payload: payload.Payload, Carried: payload.Carried}, nil
}

func sortInfraBackupEdges(edges []infraBackupEdge) {
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].IssueID != edges[j].IssueID {
			return edges[i].IssueID < edges[j].IssueID
		}
		if edges[i].DependsOnID != edges[j].DependsOnID {
			return edges[i].DependsOnID < edges[j].DependsOnID
		}
		return edges[i].Type < edges[j].Type
	})
}

// mergeInfraBackupEntries returns the union of an earlier backup and this
// pass's entries, this pass winning on a shared id, sorted by id. An entry from
// an earlier pass whose row is no longer in the work store is the only record
// of that row and is never dropped.
func mergeInfraBackupEntries(previous, current []infraBackupEntry) []infraBackupEntry {
	byID := make(map[string]infraBackupEntry, len(previous)+len(current))
	for _, e := range previous {
		byID[e.Bead.ID] = e
	}
	for _, e := range current {
		byID[e.Bead.ID] = e
	}
	out := make([]infraBackupEntry, 0, len(byID))
	for _, e := range byID {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bead.ID < out[j].Bead.ID })
	return out
}

// writeInfraRetainedBackup writes the backup atomically: temp file, fsync,
// rename, directory fsync. A crash leaves either the previous backup or the
// new one, never a truncated file read as complete.
func writeInfraRetainedBackup(target infraBindingTarget, entries []infraBackupEntry) error {
	return writeInfraRetainedBackupAt(target.Root, target.RetainedBackupPath(), target.Binding, entries)
}

// writeInfraRetainedBackupAt is writeInfraRetainedBackup for the backup at
// path in dir, labeled with binding: the city's, or a rig's.
func writeInfraRetainedBackupAt(dir, path, binding string, entries []infraBackupEntry) error {
	_, err := writeInfraBackupFile(dir, path, infraRetainedBackupFormat, binding, "retained-source backup", len(entries), func(emit func(infraBackupEntry) error) error {
		for _, entry := range entries {
			if err := emit(entry); err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

// infraBackupFileWritten is what writeInfraBackupFile wrote: its size and
// sha256.
type infraBackupFileWritten struct {
	Bytes  int64
	SHA256 string
}

// writeInfraBackupFile writes a file in the backup's line shape (a header
// naming format, binding and n, then one infraBackupEntry per row produce
// emits) atomically: temp file, fsync, rename, directory fsync. A crash
// leaves the previous file or the new one, never a truncated file read as
// complete, and a produce that emits other than n rows writes nothing. The
// retained-source backup and the storage snapshot's JSONL share it; label
// names the file in errors.
func writeInfraBackupFile(dir, path, format, binding, label string, n int, produce func(emit func(infraBackupEntry) error) error) (infraBackupFileWritten, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return infraBackupFileWritten{}, fmt.Errorf("writing the %s: %w", label, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return infraBackupFileWritten{}, fmt.Errorf("writing the %s: %w", label, err)
	}
	name := tmp.Name()
	fail := func(cause error) (infraBackupFileWritten, error) {
		_ = tmp.Close()
		_ = os.Remove(name)
		return infraBackupFileWritten{}, fmt.Errorf("writing the %s: %w", label, cause)
	}
	sum := sha256.New()
	counted := &countingWriter{target: io.MultiWriter(tmp, sum)}
	writer := bufio.NewWriter(counted)
	encoder := json.NewEncoder(writer)
	if err := encoder.Encode(infraBackupHeader{Format: format, Binding: binding, Beads: n, WrittenAt: time.Now().UTC()}); err != nil {
		return fail(err)
	}
	emitted := 0
	if err := produce(func(entry infraBackupEntry) error {
		emitted++
		return encoder.Encode(entry)
	}); err != nil {
		return fail(err)
	}
	if emitted != n {
		return fail(fmt.Errorf("the header records %d bead(s) and %d were written", n, emitted))
	}
	if err := writer.Flush(); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return infraBackupFileWritten{}, fmt.Errorf("writing the %s: %w", label, err)
	}
	if err := infraMigrationRename(name, path); err != nil {
		_ = os.Remove(name)
		return infraBackupFileWritten{}, fmt.Errorf("writing the %s: %w", label, err)
	}
	syncInfraDir(dir)
	return infraBackupFileWritten{Bytes: counted.n, SHA256: hex.EncodeToString(sum.Sum(nil))}, nil
}

// readInfraRetainedBackup reads the backup. present=false with no error means
// nothing was ever cleared from this city.
func readInfraRetainedBackup(target infraBindingTarget) ([]infraBackupEntry, bool, error) {
	return readInfraRetainedBackupAt(target.RetainedBackupPath())
}

// readInfraRetainedBackupAt reads the backup at path.
func readInfraRetainedBackupAt(path string) ([]infraBackupEntry, bool, error) {
	var entries []infraBackupEntry
	_, present, err := scanInfraBackupFile(path, infraRetainedBackupFormat, "retained-source backup", func(entry infraBackupEntry) error {
		entries = append(entries, entry)
		return nil
	})
	if err != nil || !present {
		return nil, present, err
	}
	return entries, true, nil
}

// scanInfraBackupFile reads a file in the backup's line shape, handing each
// entry to fn as it is decoded rather than holding them all. It refuses a
// header of another format, and a body whose row count is not the header's.
// present=false with no error means there is no file.
func scanInfraBackupFile(path, format, label string, fn func(infraBackupEntry) error) (int, bool, error) {
	var header infraBackupHeader
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("reading the %s %s: %w", label, path, err)
	}
	defer f.Close() //nolint:errcheck // read-only
	decoder := json.NewDecoder(bufio.NewReader(f))
	if err := decoder.Decode(&header); err != nil {
		return 0, true, fmt.Errorf("reading the %s %s: header: %w", label, path, err)
	}
	if header.Format != format {
		return 0, true, fmt.Errorf("reading the %s %s: format %q, want %q", label, path, header.Format, format)
	}
	n := 0
	for decoder.More() {
		var entry infraBackupEntry
		if err := decoder.Decode(&entry); err != nil {
			return n, true, fmt.Errorf("reading the %s %s: entry %d: %w", label, path, n+1, err)
		}
		n++
		if err := fn(entry); err != nil {
			return n, true, err
		}
	}
	if n != header.Beads {
		return n, true, fmt.Errorf("reading the %s %s: header records %d bead(s), file holds %d", label, path, header.Beads, n)
	}
	return n, true, nil
}

// verifyInfraRetainedBackup re-reads the backup from disk and proves it holds
// every entry this pass is about to remove, byte-for-byte in its encoded form.
func verifyInfraRetainedBackup(target infraBindingTarget, want []infraBackupEntry) error {
	return verifyInfraRetainedBackupAt(target.RetainedBackupPath(), want)
}

// verifyInfraRetainedBackupAt is verifyInfraRetainedBackup for the backup at
// path.
func verifyInfraRetainedBackupAt(path string, want []infraBackupEntry) error {
	got, present, err := readInfraRetainedBackupAt(path)
	if err != nil {
		return fmt.Errorf("proving the retained-source backup: %w", err)
	}
	if !present {
		return fmt.Errorf("proving the retained-source backup: %s is missing after it was written", path)
	}
	byID := make(map[string]infraBackupEntry, len(got))
	for _, e := range got {
		byID[e.Bead.ID] = e
	}
	for _, w := range want {
		g, ok := byID[w.Bead.ID]
		if !ok {
			return fmt.Errorf("proving the retained-source backup: %s does not hold %s; nothing was removed", path, w.Bead.ID)
		}
		wantJSON, err := json.Marshal(w)
		if err != nil {
			return fmt.Errorf("proving the retained-source backup: encoding %s: %w", w.Bead.ID, err)
		}
		gotJSON, err := json.Marshal(g)
		if err != nil {
			return fmt.Errorf("proving the retained-source backup: encoding %s: %w", w.Bead.ID, err)
		}
		if !bytes.Equal(wantJSON, gotJSON) {
			return fmt.Errorf("proving the retained-source backup: %s holds a different %s than the work store; nothing was removed", path, w.Bead.ID)
		}
	}
	return nil
}

// infraBackupSourceStore serves a retained-source backup as a migration
// source: the rows and every edge each row held (the boundary ones included,
// which the re-copy plans as the copy did) in a MemStore, plus the edge
// payloads the backup recorded, so the copy carries them exactly as it would
// from the work store.
type infraBackupSourceStore struct {
	*beads.MemStore
	payloads map[[2]string]string
}

// DepMetadata reports the payload the backup recorded on one edge.
func (s *infraBackupSourceStore) DepMetadata(issueID, dependsOnID string) (string, bool, error) { //nolint:unparam // the error is the beads.DepMetadataReader contract; a backup read cannot fail here
	payload, ok := s.payloads[[2]string{issueID, dependsOnID}]
	return payload, ok, nil
}

// openInfraBackupSource loads the backup as a read source for a re-copy.
func openInfraBackupSource(target infraBindingTarget) (beads.Store, error) {
	entries, present, err := readInfraRetainedBackup(target)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, fmt.Errorf("there is no retained-source backup at %s to re-copy from", target.RetainedBackupPath())
	}
	rows := make([]beads.Bead, 0, len(entries))
	var deps []beads.Dep
	payloads := map[[2]string]string{}
	// Every edge a row held, the boundary ones included: the re-copy plans
	// them as the copy did (D9), reading the far end of an edge onto work
	// from the city work store. The edges onto a row from rows that stayed
	// are not carried: the clear removed them from the work store.
	for _, e := range entries {
		rows = append(rows, e.Bead)
		for _, edge := range e.Deps {
			deps = append(deps, edge.dep())
			if edge.Carried {
				payloads[[2]string{edge.IssueID, edge.DependsOnID}] = edge.Payload
			}
		}
	}
	return &infraBackupSourceStore{MemStore: beads.NewMemStoreFrom(0, rows, deps), payloads: payloads}, nil
}

// describeInfraClear renders one clear pass for the operator.
func describeInfraClear(result infraClearResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d retained work-store cop(ies) cleared", result.Cleared)
	if result.Backup != "" {
		fmt.Fprintf(&b, " (backup: %s)", result.Backup)
	}
	if len(result.Dropped) > 0 {
		fmt.Fprintf(&b, "; %d cross-store edge(s) dropped, none re-added", len(result.Dropped))
	}
	if result.Recorded > 0 {
		fmt.Fprintf(&b, "; %d dropped cross-store edge(s) recorded on their work bead(s) (%s)", result.Recorded, beadmeta.MigratedCrossEdgesMetadataKey)
	}
	return b.String()
}

// infraDroppedCrossEdgeRecords is the gc.migrated_cross_edges entry for each
// edge from a work bead into a moved bead that the clear dropped. status
// is each moved bead's status at the clear.
func infraDroppedCrossEdgeRecords(edges []infraBackupEdge, status map[string]string) []rigCrossEdgeRecord {
	out := make([]rigCrossEdgeRecord, 0, len(edges))
	for _, e := range edges {
		out = append(out, rigCrossEdgeRecord{Dir: "out", Type: e.Type, Other: e.DependsOnID, OtherStatus: status[e.DependsOnID], Payload: e.Payload, Action: rigEdgeDropped})
	}
	return out
}

// infraClearedNote is the city-side record that this city's work store was
// cleared of its infrastructure slice. It lives in the city's .gc directory,
// NOT under the binding root, because what it guards against is the binding
// root disappearing: an unmounted volume takes the marker, the manifest and
// the backup with it, and a cleared work store then holds no infrastructure
// bead — exactly what a city with nothing to migrate looks like. Without this
// note, genesis would create a fresh, empty binding on the bare mountpoint and
// serve from it. The same note holds a revert of [storage.classes] to the work
// binding, which would otherwise start the city with no infrastructure state.
//
// It is also the clear's session record: Pending names the ids an unfinished
// clear has taken on, and Complete says the session finished — rows removed and
// cross-store edges settled.
type infraClearedNote struct {
	Binding  string `json:"binding"`
	Database string `json:"database"`
	Backup   string `json:"backup"`
	// Complete reports that the last clear finished.
	Complete bool `json:"complete"`
	// Pending names the ids an unfinished clear session has taken on.
	Pending []string `json:"pending,omitempty"`
	// LostCrossEdges names every edge from a work bead into a cleared bead
	// that the work store could not keep.
	LostCrossEdges []string `json:"lost_cross_edges,omitempty"`
}

// infraClearedNotePath is where the cleared note lives.
func infraClearedNotePath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", "storage-infra-cleared.json")
}

// readInfraClearedNote reads the cleared note. An unreadable or undecodable
// note is an error, never an absence.
func readInfraClearedNote(cityPath string) (infraClearedNote, bool, error) {
	return readInfraClearedNoteAt(infraClearedNotePath(cityPath))
}

// readInfraClearedNoteAt reads a cleared note at path: the city's, or a rig's
// (rigGraphClearedNotePath), which share one format.
func readInfraClearedNoteAt(path string) (infraClearedNote, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return infraClearedNote{}, false, nil
	}
	if err != nil {
		return infraClearedNote{}, true, fmt.Errorf("reading the cleared note %s: %w", path, err)
	}
	var note infraClearedNote
	if err := json.Unmarshal(data, &note); err != nil {
		return infraClearedNote{}, true, fmt.Errorf("decoding the cleared note %s: %w", path, err)
	}
	return note, true, nil
}

// writeInfraClearedNote records the cleared note atomically.
func writeInfraClearedNote(cityPath string, note infraClearedNote) error {
	return writeInfraClearedNoteAt(infraClearedNotePath(cityPath), note)
}

// writeInfraClearedNoteAt records a cleared note at path atomically: the
// city's, or a rig's (rigGraphClearedNotePath).
func writeInfraClearedNoteAt(path string, note infraClearedNote) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("recording the cleared note: %w", err)
	}
	data, err := json.Marshal(note)
	if err != nil {
		return fmt.Errorf("recording the cleared note: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("recording the cleared note: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("recording the cleared note: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("recording the cleared note: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("recording the cleared note: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("recording the cleared note: %w", err)
	}
	return nil
}

// infraClearedNoteHold refuses, on a binding root that carries no convergence
// marker, for a city whose work store was cleared: the binding is the only
// copy of its infrastructure state, so a missing marker means the binding is
// not where it is supposed to be — never that there is nothing to move. An
// unreadable note holds exactly as a readable one would.
func infraClearedNoteHold(cityPath string, target infraBindingTarget) error {
	data, err := os.ReadFile(infraClearedNotePath(cityPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("this city's cleared note %s cannot be read (%w), and it must hold exactly as a readable one would: the work store may no longer hold this city's infrastructure state", infraClearedNotePath(cityPath), err)
	}
	var note infraClearedNote
	if err := json.Unmarshal(data, &note); err != nil {
		return fmt.Errorf("this city's cleared note %s cannot be decoded (%w), and it must hold exactly as a readable one would: the work store may no longer hold this city's infrastructure state", infraClearedNotePath(cityPath), err)
	}
	return fmt.Errorf("this city's work store was cleared of its infrastructure beads into binding %q (%s records it, backup %s), and binding %q shows no convergence marker at %s. On a cleared city a missing marker is not evidence that there is nothing to move: the binding is the only copy of that state. If its volume is not mounted, mount it and start again — creating or copying a binding onto a bare mountpoint leaves two divergent infrastructure stores. Nothing was created or copied. Removing %s is the operator's attestation that the binding's contents are recovered or deliberately abandoned",
		note.Binding, infraClearedNotePath(cityPath), note.Backup, target.Binding, target.MarkerPath(), infraClearedNotePath(cityPath))
}
