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
// the in-tree producer). Removing the relocated row must not change what that
// work bead is waiting for, except where the wait is already over:
//
//   - a blocking edge whose binding copy is satisfied (closed, or collected by
//     the binding's GC) is released — the dependent stops waiting on a frozen
//     copy that will never close;
//   - every other cross edge is kept, re-added after the delete when the
//     backend's delete cascaded it. A kept blocking edge then names an id the
//     work store cannot resolve, which every Ready implementation reads as a
//     blocker that never clears — the same answer the frozen copy gave, and
//     the cross-class membership edge (ga-2orlf) is what lifts it. A backend
//     that refuses the re-add is reported by edge; the edge is in the backup.
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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
}

// infraClearResult is what one clear pass did.
type infraClearResult struct {
	// Cleared counts the work-store rows this pass removed.
	Cleared int
	// Backup is the path of the backup holding every row ever cleared from
	// this city, or "" when there has never been anything to clear.
	Backup string
	// Released are the blocking cross edges removed because their binding copy
	// had already satisfied them.
	Released []infraBackupEdge
	// Kept are the cross edges preserved across the delete.
	Kept []infraBackupEdge
	// Unrestorable are kept edges the backend refused to re-add after its
	// delete cascaded them, on this run. They are in the backup.
	Unrestorable []string
	// LostCrossEdges is every cross-store edge any run of this city's clears
	// could not keep, as recorded in the cleared note.
	LostCrossEdges []string
}

// infraUnprovenSourceRows is the clear's refusal for work-store infrastructure
// rows the proven copy never carried. Removing them would delete the only copy
// anything proved exists.
type infraUnprovenSourceRows struct {
	IDs []string
}

func (e *infraUnprovenSourceRows) Error() string {
	return fmt.Sprintf("the work store holds %d infrastructure bead(s) the proven copy never carried (%s), so nothing proves the binding holds them and they cannot be cleared. Copy them in with `%s` first, then run this again",
		len(e.IDs), infraStrandedIDList(e.IDs), storageRecoveryInstruction())
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
	proven, recorded, err := readInfraCopyManifest(target)
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

	rows, err := readInfraSnapshot(source)
	if err != nil {
		return result, err
	}
	if present, err := infraPathExists(target.RetainedBackupPath()); err != nil {
		return result, fmt.Errorf("reading the retained-source backup: %w", err)
	} else if present {
		result.Backup = target.RetainedBackupPath()
	}
	if len(rows) == 0 && !resuming {
		return result, nil
	}

	var unproven, retained []beads.Bead
	for _, b := range rows {
		if proven[b.ID] {
			retained = append(retained, b)
			continue
		}
		unproven = append(unproven, b)
	}
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
	entries := make([]infraBackupEntry, 0, len(retained))
	for _, b := range retained {
		if prior, ok := sessionBackup[b.ID]; ok {
			entries = append(entries, prior)
			continue
		}
		entry, err := infraBackupEntryFor(source, b, retainedIDs)
		if err != nil {
			return result, err
		}
		entries = append(entries, entry)
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

	// The cross-edge plan, over the whole session, decided against the
	// binding copy of each target. A dependent that is itself in the session
	// is not a cross edge: its row goes too.
	var release, keep []infraBackupEdge
	for _, entry := range merged {
		if !session[entry.Bead.ID] {
			continue
		}
		for _, edge := range entry.Dependents {
			if session[edge.IssueID] {
				continue
			}
			if beads.IsReadyBlockingDependencyType(edge.Type) && infraBindingCopySatisfies(held, edge.DependsOnID) {
				release = append(release, edge)
				continue
			}
			keep = append(keep, edge)
		}
	}
	announceInfraCrossEdges(announce, release, keep)

	// The city-side record, before the first row goes: from here on the work
	// store can no longer stand in for the binding, a binding root that later
	// reads as empty must not be mistaken for a city with nothing to move, and
	// a run killed from here on is resumed from the session it names.
	pending := sortedMapKeys(session)
	note = infraClearedNote{Binding: target.Binding, Database: target.Database, Backup: target.RetainedBackupPath(), Pending: pending, LostCrossEdges: note.LostCrossEdges}
	if err := writeInfraClearedNote(cityPath, note); err != nil {
		return result, err
	}

	for _, entry := range entries {
		id := entry.Bead.ID
		if err := infraClearBeforeDelete(id); err != nil {
			return result, err
		}
		for _, edge := range entry.Deps {
			if err := source.DepRemove(edge.IssueID, edge.DependsOnID); err != nil && !errors.Is(err, beads.ErrNotFound) {
				return result, fmt.Errorf("removing dep %s from the work store: %w", edge, err)
			}
		}
		if err := source.Delete(id); err != nil && !errors.Is(err, beads.ErrNotFound) {
			return result, fmt.Errorf("removing the retained copy %s from the work store: %w", id, err)
		}
		result.Cleared++
	}
	for _, edge := range release {
		if err := source.DepRemove(edge.IssueID, edge.DependsOnID); err != nil && !errors.Is(err, beads.ErrNotFound) {
			return result, fmt.Errorf("releasing satisfied dep %s: %w", edge, err)
		}
	}
	result.Released = release
	for _, edge := range keep {
		if err := infraRestoreKeptEdge(source, edge); err != nil {
			result.Unrestorable = append(result.Unrestorable, fmt.Sprintf("%s (%v)", edge, err))
			continue
		}
		result.Kept = append(result.Kept, edge)
	}

	after, err := readInfraSnapshot(source)
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
	note.LostCrossEdges = mergeSortedStrings(note.LostCrossEdges, result.Unrestorable)
	if err := writeInfraClearedNote(cityPath, note); err != nil {
		return result, err
	}
	result.LostCrossEdges = note.LostCrossEdges
	return result, nil
}

// announceInfraCrossEdges prints, before any row is removed, every edge from a
// work bead into a bead this clear removes, and what will happen to it.
func announceInfraCrossEdges(w io.Writer, release, keep []infraBackupEdge) {
	if w == nil || len(release)+len(keep) == 0 {
		return
	}
	if len(release) > 0 {
		fmt.Fprintf(w, "cross-store edges released (the binding has already satisfied the bead they wait on): %d\n", len(release)) //nolint:errcheck // best-effort output
		for _, e := range release {
			fmt.Fprintf(w, "  %s\n", e) //nolint:errcheck // best-effort output
		}
	}
	if len(keep) > 0 {
		fmt.Fprintf(w, "cross-store edges AT RISK: %d. Each is re-added after its target row is removed. A work store that cannot hold an edge to a bead it no longer has (bd and native-Dolt work stores refuse it) drops it: a blocking edge then stops holding its work bead back, and a tracks or related edge no longer links the two. Every one stays recorded in the backup, and any that is dropped is listed by `gc storage status`:\n", len(keep)) //nolint:errcheck // best-effort output
		for _, e := range keep {
			fmt.Fprintf(w, "  %s\n", e) //nolint:errcheck // best-effort output
		}
	}
}

// mergeSortedStrings returns the sorted union of a and b.
func mergeSortedStrings(a, b []string) []string {
	set := make(map[string]bool, len(a)+len(b))
	for _, s := range a {
		set[s] = true
	}
	for _, s := range b {
		set[s] = true
	}
	if len(set) == 0 {
		return nil
	}
	return sortedMapKeys(set)
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

// infraBindingCopySatisfies reports whether the binding's copy of id no longer
// blocks a dependent: it is closed with an outcome that releases dependents, as
// readiness judges it, or the binding's own GC collected it, which it does only
// to closed workflows and read mail.
func infraBindingCopySatisfies(held map[string]beads.Bead, id string) bool {
	b, ok := held[id]
	if !ok {
		return true
	}
	return beads.DependencySatisfied(b.Status, beads.ReadinessWorkOutcome(b.Metadata))
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
		return edges[i].DependsOnID < edges[j].DependsOnID
	})
}

// infraRestoreKeptEdge makes sure a kept cross edge is still in the work store
// after its target row was removed, re-adding it with its payload when the
// backend's delete cascaded it.
func infraRestoreKeptEdge(source beads.Store, edge infraBackupEdge) error {
	present, err := source.DepList(edge.IssueID, "down")
	if err != nil {
		return err
	}
	for _, d := range present {
		if d.DependsOnID == edge.DependsOnID {
			return nil
		}
	}
	if edge.Carried {
		writer, ok := source.(beads.DepMetadataWriter)
		if !ok {
			return fmt.Errorf("work store %T cannot carry the edge payload", source)
		}
		return writer.DepAddWithMetadata(edge.IssueID, edge.DependsOnID, edge.Type, edge.Payload)
	}
	return source.DepAdd(edge.IssueID, edge.DependsOnID, edge.Type)
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
	if err := os.MkdirAll(target.Root, 0o755); err != nil {
		return fmt.Errorf("writing the retained-source backup: %w", err)
	}
	tmp, err := os.CreateTemp(target.Root, infraRetainedBackupName+".tmp-*")
	if err != nil {
		return fmt.Errorf("writing the retained-source backup: %w", err)
	}
	name := tmp.Name()
	fail := func(cause error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("writing the retained-source backup: %w", cause)
	}
	writer := bufio.NewWriter(tmp)
	encoder := json.NewEncoder(writer)
	if err := encoder.Encode(infraBackupHeader{Format: infraRetainedBackupFormat, Binding: target.Binding, Beads: len(entries), WrittenAt: time.Now().UTC()}); err != nil {
		return fail(err)
	}
	for _, entry := range entries {
		if err := encoder.Encode(entry); err != nil {
			return fail(err)
		}
	}
	if err := writer.Flush(); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("writing the retained-source backup: %w", err)
	}
	if err := infraMigrationRename(name, target.RetainedBackupPath()); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("writing the retained-source backup: %w", err)
	}
	if dir, err := os.Open(target.Root); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// readInfraRetainedBackup reads the backup. present=false with no error means
// nothing was ever cleared from this city.
func readInfraRetainedBackup(target infraBindingTarget) ([]infraBackupEntry, bool, error) {
	contents, err := os.ReadFile(target.RetainedBackupPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading the retained-source backup %s: %w", target.RetainedBackupPath(), err)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	var header infraBackupHeader
	if err := decoder.Decode(&header); err != nil {
		return nil, true, fmt.Errorf("reading the retained-source backup %s: header: %w", target.RetainedBackupPath(), err)
	}
	if header.Format != infraRetainedBackupFormat {
		return nil, true, fmt.Errorf("reading the retained-source backup %s: format %q, want %q", target.RetainedBackupPath(), header.Format, infraRetainedBackupFormat)
	}
	var entries []infraBackupEntry
	for decoder.More() {
		var entry infraBackupEntry
		if err := decoder.Decode(&entry); err != nil {
			return nil, true, fmt.Errorf("reading the retained-source backup %s: entry %d: %w", target.RetainedBackupPath(), len(entries)+1, err)
		}
		entries = append(entries, entry)
	}
	if len(entries) != header.Beads {
		return nil, true, fmt.Errorf("reading the retained-source backup %s: header records %d bead(s), file holds %d", target.RetainedBackupPath(), header.Beads, len(entries))
	}
	return entries, true, nil
}

// verifyInfraRetainedBackup re-reads the backup from disk and proves it holds
// every entry this pass is about to remove, byte-for-byte in its encoded form.
func verifyInfraRetainedBackup(target infraBindingTarget, want []infraBackupEntry) error {
	got, present, err := readInfraRetainedBackup(target)
	if err != nil {
		return fmt.Errorf("proving the retained-source backup: %w", err)
	}
	if !present {
		return fmt.Errorf("proving the retained-source backup: %s is missing after it was written", target.RetainedBackupPath())
	}
	byID := make(map[string]infraBackupEntry, len(got))
	for _, e := range got {
		byID[e.Bead.ID] = e
	}
	for _, w := range want {
		g, ok := byID[w.Bead.ID]
		if !ok {
			return fmt.Errorf("proving the retained-source backup: %s does not hold %s; nothing was removed", target.RetainedBackupPath(), w.Bead.ID)
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
			return fmt.Errorf("proving the retained-source backup: %s holds a different %s than the work store; nothing was removed", target.RetainedBackupPath(), w.Bead.ID)
		}
	}
	return nil
}

// infraBackupSourceStore serves a retained-source backup as a migration
// source: the rows and their within-backup edges in a MemStore, plus the edge
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
	held := make(map[string]bool, len(entries))
	for _, e := range entries {
		held[e.Bead.ID] = true
	}
	var deps []beads.Dep
	payloads := map[[2]string]string{}
	for _, e := range entries {
		rows = append(rows, e.Bead)
		for _, edge := range e.Deps {
			if !held[edge.DependsOnID] {
				continue
			}
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
	if len(result.Released) > 0 {
		fmt.Fprintf(&b, "; %d satisfied cross-store blocking edge(s) released", len(result.Released))
	}
	if len(result.Kept) > 0 {
		fmt.Fprintf(&b, "; %d cross-store edge(s) kept", len(result.Kept))
	}
	return b.String()
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
	data, err := os.ReadFile(infraClearedNotePath(cityPath))
	if errors.Is(err, os.ErrNotExist) {
		return infraClearedNote{}, false, nil
	}
	if err != nil {
		return infraClearedNote{}, true, fmt.Errorf("reading the cleared note %s: %w", infraClearedNotePath(cityPath), err)
	}
	var note infraClearedNote
	if err := json.Unmarshal(data, &note); err != nil {
		return infraClearedNote{}, true, fmt.Errorf("decoding the cleared note %s: %w", infraClearedNotePath(cityPath), err)
	}
	return note, true, nil
}

// writeInfraClearedNote records the cleared note atomically.
func writeInfraClearedNote(cityPath string, note infraClearedNote) error {
	path := infraClearedNotePath(cityPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("recording the cleared note: %w", err)
	}
	data, err := json.Marshal(note)
	if err != nil {
		return fmt.Errorf("recording the cleared note: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "storage-infra-cleared.json.tmp-*")
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
