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
//   - an id the work store holds under a DIFFERENT bead (bd minted it again
//     since the purge) is refused; a row already back exactly as backed up is
//     skipped, so a restore can be re-run;
//   - every edge's far endpoint must be a backed-up row or a row the work
//     store holds, or the edge could not be put back;
//   - an edge carrying a payload needs a store that can write one.
//
// It restores only what a purge deleted: the backup must be one the ledger
// recorded, every row must be one the ledger recorded for that backup (by id
// and created_at), and every row must be of a purge type and classify as its
// class. A hand-built file therefore cannot use this to write arbitrary rows
// under fixed ids. This is the only sanctioned caller of
// beads.VerbatimImporter (it needs the storeclass allowlist row when T1
// lands): verbatim import bypasses id minting, and that is safe only for rows
// the store itself held before.
//
// Rows are imported verbatim (beads.VerbatimImporter: bd import keeps the
// timestamps) and re-read and compared field for field before any edge is
// written, because adding an edge may touch a row's updated_at. Then the
// outbound and inbound edges are added and re-read. The ledger records the
// rows put back, which takes them out of status's purged count and closes an
// interrupted run.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// doStorageRestorePurge is --restore-purge.
func doStorageRestorePurge(ctx context.Context, request storageOperatorRequest, backupPath string, dryRun bool, stdout, stderr io.Writer) int {
	logPrefix := storageRecoveryLogPrefix()
	session, ok := openInfraPurgeSession(ctx, request, !dryRun, logPrefix, stdout, stderr)
	if !ok {
		return 1
	}
	defer session.close()
	target, source := session.target, session.source

	backupPath = filepath.Clean(backupPath)
	if abs, err := filepath.Abs(backupPath); err == nil {
		backupPath = abs
	}
	if resolved, err := filepath.EvalSymlinks(backupPath); err == nil {
		backupPath = resolved
	}
	records, err := readInfraPurgeBackup(backupPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	ledger, err := readInfraPurgeLedger(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v. Nothing was written\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if refusal := infraRestoreScopeRefusal(ledger, backupPath, records, session.manifest.Generation); refusal != "" {
		fmt.Fprintf(stderr, "%s: %s. Nothing was written\n", logPrefix, refusal) //nolint:errcheck // best-effort stderr
		return 1
	}
	all, err := source.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		fmt.Fprintf(stderr, "%s: listing work store: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	byID := make(map[string]beads.Bead, len(all))
	for _, b := range all {
		byID[b.ID] = b
	}

	backed := make(map[string]bool, len(records))
	var toImport []beads.Bead
	var already, collisions []string
	for _, record := range records {
		want := record.Entry.Bead
		backed[want.ID] = true
		got, present := byID[want.ID]
		if !present {
			toImport = append(toImport, want)
			continue
		}
		if diff := restoredRowDifference(want, got); diff != "" {
			collisions = append(collisions, fmt.Sprintf("%s (%s)", want.ID, diff))
			continue
		}
		already = append(already, want.ID)
	}
	if len(collisions) > 0 {
		fmt.Fprintf(stderr, "%s: %d backed-up id(s) are held by the work store for a different row now, most likely because bd minted the id again since the purge; restoring would overwrite that row: %s. Nothing was written\n", //nolint:errcheck // best-effort stderr
			logPrefix, len(collisions), strings.Join(collisions, "; "))
		return 1
	}

	edges, missing := restorePurgeEdges(records, backed, byID)
	if len(missing) > 0 {
		fmt.Fprintf(stderr, "%s: %d backed-up edge(s) point at a row that is neither in the backup nor in the work store, so they cannot be put back: %s. Nothing was written\n", //nolint:errcheck // best-effort stderr
			logPrefix, len(missing), strings.Join(missing, "; "))
		return 1
	}
	writer, canPayload := source.(beads.DepMetadataWriter)
	for _, edge := range edges {
		if edge.Carried && !canPayload {
			fmt.Fprintf(stderr, "%s: edge %s carries a payload and the work store %T cannot write one. Nothing was written\n", logPrefix, edge, source) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	importer, canImport := source.(beads.VerbatimImporter)
	if !canImport && len(toImport) > 0 {
		fmt.Fprintf(stderr, "%s: the work store %T cannot import rows verbatim. Nothing was written. Restore with raw bd instead (docs/runbooks/split-storage-classes.md, \"Restoring purged mail\")\n", logPrefix, source) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "backup:  %s (%d row(s))\nrestore: %d row(s), %d already back, %d edge(s)\n", backupPath, len(records), len(toImport), len(already), len(edges)) //nolint:errcheck // best-effort stdout
	if dryRun {
		fmt.Fprintln(stdout, "dry-run: nothing was written") //nolint:errcheck // best-effort stdout
		return 0
	}

	if len(toImport) > 0 {
		if err := importer.ImportVerbatim(toImport); err != nil {
			if errors.Is(err, beads.ErrVerbatimImportUnsupported) {
				fmt.Fprintf(stderr, "%s: %v. Nothing was written. Restore with raw bd instead (docs/runbooks/split-storage-classes.md, \"Restoring purged mail\")\n", logPrefix, err) //nolint:errcheck // best-effort stderr
				return 1
			}
			fmt.Fprintf(stderr, "%s: importing the rows: %v. Run this again; rows already back are skipped\n", logPrefix, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	// The rows, proven before any edge touches them.
	after, err := source.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		fmt.Fprintf(stderr, "%s: re-reading the work store: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	afterByID := make(map[string]beads.Bead, len(after))
	for _, b := range after {
		afterByID[b.ID] = b
	}
	var differ []string
	for _, record := range records {
		want := record.Entry.Bead
		got, present := afterByID[want.ID]
		if !present {
			differ = append(differ, want.ID+" (not in the work store)")
			continue
		}
		if diff := restoredRowDifference(want, got); diff != "" {
			differ = append(differ, fmt.Sprintf("%s (%s)", want.ID, diff))
		}
	}
	if len(differ) > 0 {
		fmt.Fprintf(stderr, "%s: %d restored row(s) do not read back as backed up: %s. No edge was written; the rows imported so far stay in the work store\n", logPrefix, len(differ), strings.Join(differ, "; ")) //nolint:errcheck // best-effort stderr
		return 1
	}

	for _, edge := range edges {
		present, err := restorePurgeEdgePresent(source, edge)
		if err != nil {
			fmt.Fprintf(stderr, "%s: reading edge %s: %v\n", logPrefix, edge, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if present {
			continue
		}
		if edge.Carried {
			err = writer.DepAddWithMetadata(edge.IssueID, edge.DependsOnID, edge.Type, edge.Payload)
		} else {
			err = source.DepAdd(edge.IssueID, edge.DependsOnID, edge.Type)
		}
		if err != nil {
			fmt.Fprintf(stderr, "%s: adding edge %s: %v. Run this again; edges already back are skipped\n", logPrefix, edge, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	var lost []string
	for _, edge := range edges {
		present, err := restorePurgeEdgePresent(source, edge)
		if err != nil {
			fmt.Fprintf(stderr, "%s: reading edge %s: %v\n", logPrefix, edge, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if !present {
			lost = append(lost, edge.String())
		}
	}
	if len(lost) > 0 {
		fmt.Fprintf(stderr, "%s: %d edge(s) do not read back after the restore: %s\n", logPrefix, len(lost), strings.Join(lost, "; ")) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Recorded: every backed-up row now back that the ledger still counts as
	// purged, and (by being the last record) the end of an interrupted run.
	net := infraPurgeNetRows(ledger)
	var restoredKeys []infraPurgeRowKey
	for _, record := range records {
		key := infraPurgeRowKey{ID: record.Entry.Bead.ID, CreatedAt: record.Entry.Bead.CreatedAt.UTC().Truncate(time.Second)}
		if _, counted := net[key]; counted {
			restoredKeys = append(restoredKeys, key)
		}
	}
	_, pending := infraPurgePendingRun(ledger)
	if len(restoredKeys) > 0 || pending {
		run := ""
		for _, record := range ledger {
			if record.Backup == backupPath {
				run = record.Run
			}
		}
		if err := appendInfraPurgeLedger(target, ledger, infraPurgeLedgerRecord{
			Format: infraPurgeLedgerFormat, Event: infraPurgeEventRestored, Run: run, Type: records[0].Entry.Bead.Type,
			Backup: backupPath, Rows: restoredKeys, RecordedAt: time.Now().UTC(),
		}); err != nil {
			fmt.Fprintf(stderr, "%s: the rows and edges are back, but recording the restore failed: %v. Run this again to record it\n", logPrefix, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	fmt.Fprintf(stdout, "restored: %d row(s) and %d edge(s) into the work store; they are stranded again, and `gc storage status` names them\n", len(records), len(edges)) //nolint:errcheck // best-effort stdout
	return 0
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

// restorePurgeEdges collects every backed-up edge once, outbound and inbound,
// and names any whose far endpoint is neither backed up nor in the work store.
func restorePurgeEdges(records []infraRecoverChoiceRecord, backed map[string]bool, work map[string]beads.Bead) ([]infraBackupEdge, []string) {
	seen := map[[2]string]bool{}
	var edges []infraBackupEdge
	var missing []string
	for _, record := range records {
		for _, list := range [][]infraBackupEdge{record.Entry.Deps, record.Entry.Dependents} {
			for _, edge := range list {
				pair := [2]string{edge.IssueID, edge.DependsOnID}
				if seen[pair] {
					continue
				}
				seen[pair] = true
				for _, end := range []string{edge.IssueID, edge.DependsOnID} {
					if _, held := work[end]; !backed[end] && !held {
						missing = append(missing, fmt.Sprintf("%s (no row %s)", edge, end))
					}
				}
				edges = append(edges, edge)
			}
		}
	}
	sort.Strings(missing)
	return edges, missing
}

// restorePurgeEdgePresent reports whether the work store holds edge, with its
// type.
func restorePurgeEdgePresent(source beads.Store, edge infraBackupEdge) (bool, error) {
	deps, err := source.DepList(edge.IssueID, "down")
	if err != nil {
		return false, err
	}
	for _, d := range deps {
		if d.DependsOnID == edge.DependsOnID && (d.Type == edge.Type || (d.Type == "" && edge.Type == "blocks") || (edge.Type == "" && d.Type == "blocks")) {
			return true, nil
		}
	}
	return false, nil
}

// infraRestoreScopeRefusal names why the backup's rows are not all rows a
// purge recorded deleting under it, or returns "" when they are.
func infraRestoreScopeRefusal(ledger []infraPurgeLedgerRecord, backupPath string, records []infraRecoverChoiceRecord, gen coordclass.Generation) string {
	recorded := map[infraPurgeRowKey]bool{}
	known := false
	for _, record := range ledger {
		if filepath.Clean(record.Backup) != backupPath {
			continue
		}
		known = true
		if record.Event != infraPurgeEventBackedUp && record.Event != infraPurgeEventPurged {
			continue
		}
		for _, key := range record.Rows {
			key.CreatedAt = key.CreatedAt.UTC().Truncate(time.Second)
			recorded[key] = true
		}
	}
	if !known {
		return fmt.Sprintf("%s is not a backup the purge ledger records; --%s restores only what a purge deleted", backupPath, strandedRestorePurgeFlag)
	}
	if len(records) == 0 {
		return fmt.Sprintf("%s holds no rows", backupPath)
	}
	var stray []string
	for _, record := range records {
		b := record.Entry.Bead
		key := infraPurgeRowKey{ID: b.ID, CreatedAt: b.CreatedAt.UTC().Truncate(time.Second)}
		class, purgeable := infraPurgeTypes[b.Type]
		switch {
		case !recorded[key]:
			stray = append(stray, fmt.Sprintf("%s (the ledger records no purge of this row under this backup)", key))
		case !purgeable:
			stray = append(stray, fmt.Sprintf("%s (type %q is not a purge type)", b.ID, b.Type))
		case coordclass.ClassifyAt(b, gen) != class:
			stray = append(stray, fmt.Sprintf("%s (classifies as %s, not %s)", b.ID, coordclass.ClassifyAt(b, gen), class))
		}
	}
	if len(stray) > 0 {
		sort.Strings(stray)
		return fmt.Sprintf("%d row(s) of %s are not rows a purge deleted: %s", len(stray), backupPath, strings.Join(stray, "; "))
	}
	return ""
}
