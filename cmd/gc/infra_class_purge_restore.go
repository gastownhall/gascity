package main

// `gc storage recover-stranded --from-work --restore-purge <backup>`: put the
// rows a --purge-type run deleted back into the work store, exactly as they
// were, with every edge that touched them.
//
// It exists because the generic route does not work on a split city: `gc bd
// import` refuses rows of a class the work store does not hold, and with
// --skip-infra it imports none of them. The rows go back where they were —
// the work store — and are stranded again, which `gc storage status` then
// reports, so the operator is back exactly where the purge started.
//
// Nothing is written until every check passes:
//
//   - a row the binding holds as the same bead is refused: it was carried into
//     the binding after the purge backed it up, so the binding serves it, and
//     this command writes only the work store. It would put the row's edges
//     back on the work store's copy and report a rollback the city never
//     reads (infraPurgeRestoreBindingRefusal);
//   - an id the work store holds under a DIFFERENT bead (bd minted it again
//     since the purge) is refused, naming the step that frees it, as is a row
//     changed since the backup; a row already back as backed up is skipped,
//     so a restore can be re-run. The parent a store reads off the row's
//     parent-child edge is left out of that comparison
//     (restoredRowDifferenceBeforeEdges): the purge removed the edge, and the
//     restore adds it back;
//   - every edge's far endpoint must be a backed-up row or a row the work
//     store holds, or the edge could not be put back;
//   - edges are read back through the work store's relation listing, so a
//     store that refuses it is refused when there is an edge to restore, as
//     the purge refuses it;
//   - an edge carrying a payload needs a store that can write one, and rows to
//     import need a store that imports verbatim, each decided on the store
//     under the policy wrapper (infraPurgeStores.underPolicy), so --dry-run
//     refuses where the restore would.
//
// It restores only what a purge deleted: the backup must be one the ledger
// recorded, every row must be one the ledger recorded for that backup (by id
// and created_at), the backup must hold each row the ledger recorded for it
// exactly once, every row must be of a purge type and classify as its
// class, and every edge must touch its record's row. A hand-built file
// therefore cannot use this to write arbitrary rows under fixed ids, or an
// edge between rows the purge never touched. This is the only sanctioned
// caller of beads.VerbatimImporter, reached through beads.As on the handle
// openInfraPurgeStores mints: verbatim import bypasses id minting, and that is
// safe only for rows the store itself held before. The backup is matched to
// its ledger record by location, not by spelling: the ledger names it under
// the binding root's canonical spelling, and on macOS an operator's
// /private/var/... path names the ledger's /var/....
//
// While a purge run is interrupted, only that run's backup is restored: rows
// of another run put back now would be selected again by the interrupted
// run's resume, which its backup does not hold.
//
// Rows are imported verbatim (beads.VerbatimImporter: bd import keeps the
// timestamps) and re-read and compared field for field before any edge is
// written, so a row that does not read back leaves no edge written. bd keeps
// no parent of its own: it reads one off the row's parent-child edge, and bd
// import drops it, so that comparison leaves out the parent the row's
// backed-up edges imply. Then the outbound and inbound edges are added, each
// with the payload it carried, and re-read; an edge already back without its
// payload is written again (infraPurgeEdgeDifference). Then the rows are
// re-read and compared again, whole, parent included. The ledger records the
// rows put back, which takes them out of status's purged count and closes an
// interrupted run.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// doStorageRestorePurge is --restore-purge.
func doStorageRestorePurge(ctx context.Context, request storageOperatorRequest, backupPath string, dryRun bool, stdout, stderr io.Writer) int {
	logPrefix := storageRecoveryLogPrefix()
	session, ok := openInfraPurgeSession(ctx, request, !dryRun, logPrefix, stdout, stderr)
	if !ok {
		return 1
	}
	defer session.close()

	ledger, err := readInfraPurgeLedger(session.target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v. Nothing was written\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	run, records, err := infraPurgeRestoreScope(ledger, backupPath, session.manifest.Generation)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v. Nothing was written\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := infraPurgeRestoreBindingRefusal(session.stores, session.target, run, records); err != nil {
		fmt.Fprintf(stderr, "%s: %v. Nothing was written\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	pending, interrupted := infraPurgePendingRun(ledger)
	plan, err := planInfraPurgeRestore(session.stores, records, interrupted && pending.Run == run.Run)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v. Nothing was written\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "backup:  %s (%d row(s))\nrestore: %d row(s), %d already back, %d edge(s)\n", run.Backup, len(records), len(plan.toImport), plan.already, len(plan.edges)) //nolint:errcheck // best-effort stdout
	if dryRun {
		fmt.Fprintln(stdout, "dry-run: nothing was written") //nolint:errcheck // best-effort stdout
		return 0
	}
	if err := applyInfraPurgeRestore(session.stores, records, plan); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := recordInfraPurgeRestore(session.target, ledger, run, records); err != nil {
		fmt.Fprintf(stderr, "%s: the rows and edges are back, but recording the restore failed: %v. Run this again to record it\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "restored: %d row(s) and %d edge(s) into the work store; they are stranded again, and `gc storage status` names them\n", len(records), len(plan.edges)) //nolint:errcheck // best-effort stdout
	return 0
}

// infraPurgeRestoreScope returns the ledger's backed-up record of the run whose
// backup the restore names, and the backup's records, or why the restore
// refuses them.
func infraPurgeRestoreScope(ledger []infraPurgeLedgerRecord, backupPath string, gen coordclass.Generation) (infraPurgeLedgerRecord, []infraRecoverChoiceRecord, error) {
	run, known := infraPurgeRunOfBackup(ledger, backupPath)
	if !known {
		return run, nil, fmt.Errorf("%s is not a backup the purge ledger records; --%s restores only what a purge deleted", backupPath, strandedRestorePurgeFlag)
	}
	if pending, interrupted := infraPurgePendingRun(ledger); interrupted && pending.Run != run.Run {
		return run, nil, fmt.Errorf("the purge run %s is interrupted (its backup is %s), and %s is another run's backup: its rows put back now would join the interrupted run's selection, which that run's backup does not hold, so the run could not resume. Finish that run first by running the purge again, or restore it with --%s %s",
			pending.Run, pending.Backup, run.Backup, strandedRestorePurgeFlag, pending.Backup)
	}
	records, err := readInfraPurgeBackup(run.Backup)
	if err != nil {
		return run, nil, err
	}
	if refusal := infraRestoreScopeRefusal(run, records, gen); refusal != "" {
		return run, nil, errors.New(refusal)
	}
	return run, records, nil
}

// infraPurgeRunOfBackup returns the ledger's backed-up record of the run whose
// backup is the file at path. The ledger names each backup as the purge wrote
// it, under the binding root's canonical spelling, and path may name the same
// file another way (through a symlink, or on macOS as /private/var/... for
// /var/...), so the two are compared by location.
func infraPurgeRunOfBackup(ledger []infraPurgeLedgerRecord, path string) (infraPurgeLedgerRecord, bool) {
	for _, record := range ledger {
		if record.Event == infraPurgeEventBackedUp && pathutil.SamePath(record.Backup, path) {
			return record, true
		}
	}
	return infraPurgeLedgerRecord{}, false
}

// infraPurgeRestoreBindingRefusal says why records must not be restored
// because the binding holds some of their rows as the same bead, or returns
// nil when it holds none. Such a row was carried into the binding after run
// backed it up, by a recover-stranded that did not know about the purge or by
// hand, so the binding serves it. The restore writes only the work store, so
// it would put the row's edges back on the work store's copy, which status
// counts as a retained copy, and never where the city reads them.
func infraPurgeRestoreBindingRefusal(stores *infraPurgeStores, target infraBindingTarget, run infraPurgeLedgerRecord, records []infraRecoverChoiceRecord) error {
	binding, closeBinding, err := stores.openBinding()
	if err != nil {
		return fmt.Errorf("opening binding %s: %w", target.Database, err)
	}
	defer closeBinding()
	rows := make([]beads.Bead, 0, len(records))
	for _, record := range records {
		rows = append(rows, record.Entry.Bead)
	}
	held, err := infraPurgeRowsTheBindingHolds(binding, rows)
	if err != nil {
		return err
	}
	if len(held) == 0 {
		return nil
	}
	return fmt.Errorf("the binding holds %d row(s) of %s as the same bead (the same id and created_at): %s. They were carried into it after the purge backed them up, so the binding serves them, and --%s, which writes only the work store, cannot put their edges back where the city reads them. Roll back by hand instead (docs/runbooks/split-storage-classes.md, \"Restoring purged mail\")",
		len(held), run.Backup, infraStrandedIDList(held), strandedRestorePurgeFlag)
}

// infraPurgeRestorePlan is what a restore writes, and the capabilities it
// writes through, decided on the store under the policy wrapper.
type infraPurgeRestorePlan struct {
	// toImport are the backed-up rows the work store does not hold.
	toImport []beads.Bead
	// already counts the backed-up rows the work store holds as backed up.
	already int
	// edges are every backed-up edge, once.
	edges []infraBackupEdge
	// importer imports toImport; nil when the store cannot, which the plan
	// refuses whenever there is a row to import.
	importer beads.VerbatimImporter
	// writer adds an edge with its payload; nil when the store cannot, which
	// the plan refuses whenever an edge carries one.
	writer beads.DepMetadataWriter
}

// planInfraPurgeRestore decides what the restore of records writes, or names
// why it refuses before writing anything. interrupted is whether records are
// the interrupted run's backup, which decides the step a refusal names.
func planInfraPurgeRestore(stores *infraPurgeStores, records []infraRecoverChoiceRecord, interrupted bool) (infraPurgeRestorePlan, error) {
	var plan infraPurgeRestorePlan
	all, err := stores.work.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return plan, fmt.Errorf("listing work store: %w", err)
	}
	byID := make(map[string]beads.Bead, len(all))
	for _, b := range all {
		byID[b.ID] = b
	}
	backed := make(map[string]bool, len(records))
	var reminted, changed []string
	for _, record := range records {
		want := record.Entry.Bead
		backed[want.ID] = true
		got, present := byID[want.ID]
		if !present {
			plan.toImport = append(plan.toImport, want)
			continue
		}
		diff := restoredRowDifferenceBeforeEdges(record.Entry, got)
		switch {
		case diff == "":
			plan.already++
		case !sameCreatedSecond(want.CreatedAt, got.CreatedAt):
			reminted = append(reminted, fmt.Sprintf("%s (%s)", want.ID, diff))
		default:
			changed = append(changed, fmt.Sprintf("%s (%s)", want.ID, diff))
		}
	}
	if err := infraPurgeRestoreCollisionRefusal(reminted, changed, interrupted); err != nil {
		return plan, err
	}
	var missing []string
	plan.edges, missing = restorePurgeEdges(records, backed, byID)
	if len(missing) > 0 {
		return plan, fmt.Errorf("%d backed-up edge(s) point at a row that is neither in the backup nor in the work store, so they cannot be put back: %s", len(missing), strings.Join(missing, "; "))
	}
	if len(plan.edges) > 0 {
		if err := infraPurgeRelationListingRefusal(stores.work, plan.edges[0].IssueID); err != nil {
			return plan, err
		}
	}
	if writer, ok := beads.As[beads.DepMetadataWriter](stores.underPolicy); ok {
		plan.writer = writer
	}
	for _, edge := range plan.edges {
		if edge.Carried && plan.writer == nil {
			return plan, fmt.Errorf("edge %s carries a payload and the work store %s cannot write one", edge, stores.engine)
		}
	}
	if importer, ok := beads.As[beads.VerbatimImporter](stores.underPolicy); ok {
		plan.importer = importer
	} else if len(plan.toImport) > 0 {
		return plan, fmt.Errorf("the work store %s cannot import rows verbatim; restore with raw bd instead (docs/runbooks/split-storage-classes.md, \"Restoring purged mail\")", stores.engine)
	}
	return plan, nil
}

// infraPurgeRestoreCollisionRefusal says why the rows the work store holds
// under backed-up ids stop the restore, or returns nil when none does.
// reminted are ids bd minted again for a different bead since the purge, and
// changed are backed-up beads that changed since; restoring would overwrite
// either. A reused id frees only once its new bead leaves the work store, and
// while the backup is the interrupted run's, finishing that run is what closes
// it: the purge leaves the new bead in place.
func infraPurgeRestoreCollisionRefusal(reminted, changed []string, interrupted bool) error {
	var parts []string
	if len(reminted) > 0 {
		step := "Move the new bead(s) out of the work store first (a stranded message goes with a purge), then restore"
		if interrupted {
			step = "Finish the interrupted run instead by running the purge again: it leaves the new bead(s) in place, and once a later purge has taken them, this backup restores"
		}
		parts = append(parts, fmt.Sprintf("%d backed-up id(s) are held by the work store for a different bead now (bd minted the id again since the purge), and restoring would overwrite that bead: %s. %s",
			len(reminted), strings.Join(reminted, "; "), step))
	}
	if len(changed) > 0 {
		parts = append(parts, fmt.Sprintf("%d backed-up row(s) are in the work store as the same bead but changed since the purge backed them up, and restoring would overwrite the change: %s",
			len(changed), strings.Join(changed, "; ")))
	}
	if len(parts) == 0 {
		return nil
	}
	return errors.New(strings.Join(parts, ". "))
}

// infraPurgeRelationListingRefusal returns why the restore cannot read the
// edges it writes back on source, or nil when it can: a work store that
// refuses relation listing outright (infraRelationCapabilityRefusal), which
// the purge refuses too (infraPurgeInboundEdges). Any other probe error is
// left to the reads themselves, as newSourceDepReader leaves it: a moment's
// failure is no evidence about the store.
func infraPurgeRelationListingRefusal(source beads.WorkStore, probeID string) error {
	if _, err := source.DepList(probeID, "down"); infraRelationCapabilityRefusal(err) {
		return fmt.Errorf("the work store refuses relation listing (%w), and the restore reads every edge it writes back through it; restore with raw bd instead (docs/runbooks/split-storage-classes.md, \"Restoring purged mail\")", err)
	}
	return nil
}

// applyInfraPurgeRestore imports the plan's rows and proves them, then adds
// the plan's edges and proves them, and the rows again with their edges. Its
// error says what is already back.
func applyInfraPurgeRestore(stores *infraPurgeStores, records []infraRecoverChoiceRecord, plan infraPurgeRestorePlan) error {
	if len(plan.toImport) > 0 {
		if err := plan.importer.ImportVerbatim(plan.toImport); err != nil {
			if errors.Is(err, beads.ErrVerbatimImportUnsupported) {
				return fmt.Errorf("%w. Nothing was written. Restore with raw bd instead (docs/runbooks/split-storage-classes.md, \"Restoring purged mail\")", err)
			}
			return fmt.Errorf("importing the rows: %w. Run this again; rows already back are skipped", err)
		}
	}
	// The rows, proven before any edge touches them.
	differ, err := infraPurgeRowsNotBack(stores.work, records, restoredRowDifferenceBeforeEdges)
	if err != nil {
		return err
	}
	if len(differ) > 0 {
		return fmt.Errorf("%d restored row(s) do not read back as backed up: %s. No edge was written; the rows imported so far stay in the work store", len(differ), strings.Join(differ, "; "))
	}

	for _, edge := range plan.edges {
		diff, err := infraPurgeEdgeDifference(stores, edge)
		if err != nil {
			return fmt.Errorf("reading edge %s: %w", edge, err)
		}
		if diff == "" {
			continue
		}
		if edge.Carried {
			err = plan.writer.DepAddWithMetadata(edge.IssueID, edge.DependsOnID, edge.Type, edge.Payload)
		} else {
			err = stores.work.DepAdd(edge.IssueID, edge.DependsOnID, edge.Type)
		}
		if err != nil {
			return fmt.Errorf("adding edge %s: %w. Run this again; edges already back are skipped", edge, err)
		}
	}
	var lost []string
	for _, edge := range plan.edges {
		diff, err := infraPurgeEdgeDifference(stores, edge)
		if err != nil {
			return fmt.Errorf("reading edge %s: %w", edge, err)
		}
		if diff != "" {
			lost = append(lost, fmt.Sprintf("%s (%s)", edge, diff))
		}
	}
	if len(lost) > 0 {
		return fmt.Errorf("%d edge(s) do not read back after the restore: %s", len(lost), strings.Join(lost, "; "))
	}
	// The rows again, whole, now that their edges are back.
	differ, err = infraPurgeRowsNotBack(stores.work, records, restoredEntryDifference)
	if err != nil {
		return err
	}
	if len(differ) > 0 {
		return fmt.Errorf("%d restored row(s) do not read back as backed up once their edges are back: %s. The rows and edges stay in the work store", len(differ), strings.Join(differ, "; "))
	}
	return nil
}

// infraPurgeRowsNotBack re-reads the work store and names every row of records
// it does not hold, or holds other than as backed up by differ.
func infraPurgeRowsNotBack(source beads.WorkStore, records []infraRecoverChoiceRecord, differ func(infraBackupEntry, beads.Bead) string) ([]string, error) {
	rows, err := source.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return nil, fmt.Errorf("re-reading the work store: %w", err)
	}
	byID := make(map[string]beads.Bead, len(rows))
	for _, b := range rows {
		byID[b.ID] = b
	}
	var notBack []string
	for _, record := range records {
		id := record.Entry.Bead.ID
		got, present := byID[id]
		if !present {
			notBack = append(notBack, id+" (not in the work store)")
			continue
		}
		if diff := differ(record.Entry, got); diff != "" {
			notBack = append(notBack, fmt.Sprintf("%s (%s)", id, diff))
		}
	}
	return notBack, nil
}

// recordInfraPurgeRestore records the restore of run's backup: every row of it
// now back that the ledger still counts as purged, and, while run is the
// interrupted one, the end of that run. The record names the backup as the
// ledger does.
func recordInfraPurgeRestore(target infraBindingTarget, ledger []infraPurgeLedgerRecord, run infraPurgeLedgerRecord, records []infraRecoverChoiceRecord) error {
	net := infraPurgeNetRows(ledger)
	var restored []infraPurgeRowKey
	for _, record := range records {
		key := infraPurgeSecondKey(record.Entry.Bead.ID, record.Entry.Bead.CreatedAt)
		if _, counted := net[key]; counted {
			restored = append(restored, key)
		}
	}
	pending, interrupted := infraPurgePendingRun(ledger)
	if len(restored) == 0 && (!interrupted || pending.Run != run.Run) {
		return nil
	}
	return appendInfraPurgeLedger(target, ledger, infraPurgeLedgerRecord{
		Format: infraPurgeLedgerFormat, Event: infraPurgeEventRestored, Run: run.Run, Type: run.Type,
		Backup: run.Backup, Rows: restored, RecordedAt: time.Now().UTC(),
	})
}

// restoredRowDifference names how got, the work store's row under want's id,
// differs from the backed-up row, or returns "" when it is that row.
func restoredRowDifference(want, got beads.Bead) string {
	if !sameCreatedSecond(want.CreatedAt, got.CreatedAt) {
		return fmt.Sprintf("created %s, backed up as created %s", got.CreatedAt.UTC().Format(time.RFC3339), want.CreatedAt.UTC().Format(time.RFC3339))
	}
	got.IsBlocked = nil
	return beadCopyDifference(want, got)
}

// restoredEntryDifference is restoredRowDifference of entry's row, the whole
// row: the comparison once the row's edges are back.
func restoredEntryDifference(entry infraBackupEntry, got beads.Bead) string {
	return restoredRowDifference(entry.Bead, got)
}

// restoredRowDifferenceBeforeEdges is restoredRowDifference of entry's row
// while its edges are not back: both rows are compared without the state a
// store derives from the row's backed-up edges (infraPurgeRowWithoutEdgeState).
// bd keeps no parent of its own: it reads one off the row's parent-child edge,
// and bd import drops it, so a row the purge stripped of its edges, or one the
// restore imported, reads back with no parent until the edge is added again.
func restoredRowDifferenceBeforeEdges(entry infraBackupEntry, got beads.Bead) string {
	return restoredRowDifference(infraPurgeRowWithoutEdgeState(entry.Bead, entry.Deps), infraPurgeRowWithoutEdgeState(got, entry.Deps))
}

// restorePurgeEdges collects every backed-up edge once, outbound and inbound,
// and names any whose far endpoint is neither backed up nor in the work store.
func restorePurgeEdges(records []infraRecoverChoiceRecord, backed map[string]bool, work map[string]beads.Bead) ([]infraBackupEdge, []string) {
	edges := infraPurgeEdges(infraPurgeRecordEntries(records))
	var missing []string
	for _, edge := range edges {
		for _, end := range []string{edge.IssueID, edge.DependsOnID} {
			if _, held := work[end]; !backed[end] && !held {
				missing = append(missing, fmt.Sprintf("%s (no row %s)", edge, end))
			}
		}
	}
	sort.Strings(missing)
	return edges, missing
}

// infraRestoreScopeRefusal names why records are not exactly the rows run
// recorded backing up, each once, of a purge type and classifying as its
// class, each with only edges that touch it (infraRestoreForeignEdges), or
// returns "" when they are. A backup missing a recorded row would restore
// part of the run, and the ledger would still count the rest as purged with
// no copy left to put back.
func infraRestoreScopeRefusal(run infraPurgeLedgerRecord, records []infraRecoverChoiceRecord, gen coordclass.Generation) string {
	if len(records) == 0 {
		return fmt.Sprintf("%s holds no rows", run.Backup)
	}
	recorded := make(map[infraPurgeRowKey]bool, len(run.Rows))
	for _, key := range run.Rows {
		recorded[infraPurgeSecondKey(key.ID, key.CreatedAt)] = true
	}
	held := make(map[infraPurgeRowKey]bool, len(records))
	var stray, uncovered []string
	for _, record := range records {
		b := record.Entry.Bead
		key := infraPurgeSecondKey(b.ID, b.CreatedAt)
		class, purgeable := infraPurgeTypes[b.Type]
		switch {
		case held[key]:
			uncovered = append(uncovered, fmt.Sprintf("%s (held more than once)", key))
		case !recorded[key]:
			stray = append(stray, fmt.Sprintf("%s (the ledger records no purge of this row under this backup)", key))
		case !purgeable:
			stray = append(stray, fmt.Sprintf("%s (type %q is not a purge type)", b.ID, b.Type))
		case coordclass.ClassifyAt(b, gen) != class:
			stray = append(stray, fmt.Sprintf("%s (classifies as %s, not %s)", b.ID, coordclass.ClassifyAt(b, gen), class))
		}
		held[key] = true
		stray = append(stray, infraRestoreForeignEdges(record.Entry)...)
	}
	for key := range recorded {
		if !held[key] {
			uncovered = append(uncovered, fmt.Sprintf("%s (not held)", key))
		}
	}
	var parts []string
	if len(stray) > 0 {
		sort.Strings(stray)
		parts = append(parts, fmt.Sprintf("%d row(s) or edge(s) of %s are not what a purge deleted: %s", len(stray), run.Backup, strings.Join(stray, "; ")))
	}
	if len(uncovered) > 0 {
		sort.Strings(uncovered)
		parts = append(parts, fmt.Sprintf("%s does not hold each row the ledger records backing up under it exactly once, so it is not the backup that purge wrote: %s", run.Backup, strings.Join(uncovered, "; ")))
	}
	return strings.Join(parts, ". ")
}

// infraRestoreForeignEdges names every edge of entry that does not touch its
// row: an outbound edge starts at the row and an inbound edge ends at it, as
// infraPurgeBackupEntries records them. An edge between two other rows is not
// one the purge removed.
func infraRestoreForeignEdges(entry infraBackupEntry) []string {
	var foreign []string
	for _, edge := range entry.Deps {
		if edge.IssueID != entry.Bead.ID {
			foreign = append(foreign, fmt.Sprintf("%s (an outbound edge of %s that does not start at it)", edge, entry.Bead.ID))
		}
	}
	for _, edge := range entry.Dependents {
		if edge.DependsOnID != entry.Bead.ID {
			foreign = append(foreign, fmt.Sprintf("%s (an inbound edge of %s that does not end at it)", edge, entry.Bead.ID))
		}
	}
	return foreign
}
