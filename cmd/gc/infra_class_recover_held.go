package main

// A work-store row whose id the binding holds for a different bead.
//
// bd reuses short ids. Once the copy has carried a bead and the work store has
// let go of its row, the work store can mint the same id again for an
// unrelated bead. When an infrastructure bead lands under that id and the
// binding still holds the bead the copy carried, nothing can carry the new row
// under its id: the id is taken. The containment check names it stranded
// (infra_copy_manifest.go keys the manifest by (id, created_at) so it is never
// read as the carried bead), recover-stranded finds the binding's row is
// another bead, and the clear refuses to delete a row nothing carried. Left at
// that, the city refuses to boot and no command clears it.
//
// "Another bead" is decided by identity, with the manifest's own key: the
// binding's row is this bead when it was created in the same second, or, for
// a work row that carries no created_at, when a copy out of the work store
// wrote it (infraHeldRowIsThisBead). Its fields decide nothing, because the
// binding has served the row since it was carried and a live city changes it:
// mail is read and closed, a session moves on. Such a row is the same bead —
// the binding can read it, so the containment check retains it, and
// recover-stranded records it — and neither choice below is offered for it.
//
// Which bead should keep the id is not something this program can know: the
// binding's row is the live bead the city has served since cutover, and the
// work-store row is a later write that only one store can hold. So the operator
// answers per id, and recover-stranded acts on the answer:
//
//   - --replace-binding-row <id>: the work-store row replaces the binding's. The
//     binding's row, with every edge touching it, is backed up and removed, and
//     the work-store row is then moved like any stranded row: copied, proven
//     equal against the reopened binding, and recorded with its created_at. The
//     clear later removes the work-store copy, as it does every carried row.
//   - --drop-work-row <id>: the binding's row stays the authority. The
//     work-store row, with every edge touching it, is backed up and removed;
//     the manifest does not change.
//
// These are the only writes recover-stranded makes that remove a row, and each
// removed row is written to infra.recover-choices.jsonl at the binding root,
// re-read and proven, before the first removal. The backup is not the clear's
// retained-source backup on purpose: that one is keyed by id and feeds a
// re-copy, and the two rows a choice decides between share their id.
//
// Every edge touching the removed row goes with it rather than being kept on
// the row that keeps the id. Such an edge was written against one bead; kept,
// it would silently point at the other.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// The per-id choices for a work-store row whose id the binding holds for a
// different bead.
const (
	strandedReplaceBindingRowFlag = "replace-binding-row"
	strandedDropWorkRowFlag       = "drop-work-row"
)

// infraRecoverChoicesBackupName is the backup of every row a per-id choice
// removed, at the binding root beside the manifest.
const infraRecoverChoicesBackupName = "infra.recover-choices.jsonl"

// infraRecoverChoicesFormat versions one backup line. A reader refuses any
// other value rather than guess at a shape it was not written for.
const infraRecoverChoicesFormat = "gc.infra.recover-choice/v1"

// The store a backed-up row was removed from.
const (
	infraRecoverChoiceStoreBinding = "binding"
	infraRecoverChoiceStoreWork    = "work"
)

// RecoverChoicesBackupPath is where recover-stranded records the rows its
// per-id choices removed.
func (t infraBindingTarget) RecoverChoicesBackupPath() string {
	return filepath.Join(t.Root, infraRecoverChoicesBackupName)
}

// infraRecoverChoiceRecord is one removed row as the backup records it: the
// row and every edge touching it in the store it was removed from.
type infraRecoverChoiceRecord struct {
	Format string `json:"format"`
	// Choice is the flag that removed the row.
	Choice string `json:"choice"`
	// Store is the store the row was removed from (binding or work).
	Store      string           `json:"store"`
	RecordedAt time.Time        `json:"recorded_at"`
	Entry      infraBackupEntry `json:"entry"`
}

// readInfraHeldRow reads the binding's row under id, an id the binding listed
// a moment ago. held is false, with no error, when the binding no longer
// holds it: its own GC collected the row since. Any other failed read is an
// error, never "the same bead": that guess hides the per-id choice for a row
// the binding holds for a different bead. The containment check and
// recover-stranded both read through it, so they read a held id alike.
func readInfraHeldRow(destination beads.Store, id string) (row beads.Bead, held bool, err error) {
	row, err = getInfraHeldRow(destination, id)
	if errors.Is(err, beads.ErrNotFound) {
		return beads.Bead{}, false, nil
	}
	if err != nil {
		return beads.Bead{}, false, fmt.Errorf("reading the binding's row under %s: %w", id, err)
	}
	return row, true, nil
}

// getInfraHeldRow is the read readInfraHeldRow makes: a seam, so a test can
// stand in a binding whose read fails.
var getInfraHeldRow = func(destination beads.Store, id string) (beads.Bead, error) {
	return destination.Get(id)
}

// infraHeldRowIsThisBead reports whether held, the binding's row under b's
// id, is b itself (infraHeldRowDifference).
func infraHeldRowIsThisBead(b, held beads.Bead) bool {
	return infraHeldRowDifference(b, held) == ""
}

// infraHeldRowDifference names why held, the binding's row under b's id, is a
// different bead from b, or returns "" when it is b. Two rows under one id
// created in the same second are one bead, however far the binding's copy has
// moved on since.
//
// A work row with no created_at cannot share that key: the binding's insert
// stamps its own clock where a copied row carries none. Its proof is the
// provenance stamp instead, which only a copy out of the work store puts on a
// row (infraMigrationRow). No bead is minted with no created_at, because every
// store's create stamps one, so a stamped copy under such a row's id was made
// from that very row. The fields decide nothing here either: the binding has
// served the row since.
func infraHeldRowDifference(b, held beads.Bead) string {
	switch {
	case b.CreatedAt.IsZero():
		if held.Metadata[infraMigrationStampKey] != "" {
			return ""
		}
		return fmt.Sprintf("this row carries no created_at, and the binding's row under the id carries no %s stamp, so no copy out of the work store wrote it: it is a different bead",
			infraMigrationStampKey)
	case sameCreatedSecond(b.CreatedAt, held.CreatedAt):
		return ""
	default:
		return fmt.Sprintf("the binding's row under the id was created %s and this one %s, so it is a different bead",
			held.CreatedAt.UTC().Format(time.RFC3339), b.CreatedAt.UTC().Format(time.RFC3339))
	}
}

// infraHeldDifferentAdvice is the sentence every surface that names such rows
// carries: what they are, and the two answers.
func infraHeldDifferentAdvice(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return fmt.Sprintf("%d of them (%s) reuse an id the binding holds for a DIFFERENT bead: the work store minted the id again after the copy carried the original, so no copy can carry them under that id. Decide per id and run `%s` with --%s <id> (the work-store row replaces the binding's) or --%s <id> (the work-store row is removed and the binding's stays); each backs up the row it removes, with its edges, to %s at the binding root first.",
		len(ids), infraStrandedIDList(ids), storageRecoveryInstruction(), strandedReplaceBindingRowFlag, strandedDropWorkRowFlag, infraRecoverChoicesBackupName)
}

// reportInfraHeldDifferent prints, for each row awaiting a choice, the
// work-store row and the binding's row under its id: the evidence the choice
// is made on. It does not lean toward either answer.
func reportInfraHeldDifferent(w io.Writer, rows []beads.Bead, held map[string]beads.Bead) {
	describe := func(side string, b beads.Bead) {
		fmt.Fprintf(w, "    %-8s status=%s  type=%s  created=%s  updated=%s  title=%q\n", //nolint:errcheck // best-effort output
			side, b.Status, b.Type, b.CreatedAt.UTC().Format(time.RFC3339), b.UpdatedAt.UTC().Format(time.RFC3339), b.Title)
	}
	for _, b := range rows {
		fmt.Fprintf(w, "  %s\n", b.ID) //nolint:errcheck // best-effort output
		describe("work:", b)
		describe("binding:", held[b.ID])
	}
}

// applyInfraHeldChoices carries out the operator's per-id choices: it backs up
// every row it is about to remove, proves the backup from disk, and only then
// removes the binding's rows (replace) and the work-store rows (drop). The
// replaced ids' work-store rows are then moved by the caller.
func applyInfraHeldChoices(target infraBindingTarget, source, destination beads.Store, replace, drop []beads.Bead, held map[string]beads.Bead) error {
	records := make([]infraRecoverChoiceRecord, 0, len(replace)+len(drop))
	now := time.Now().UTC()
	for _, b := range replace {
		entry, err := infraBackupEntryFor(destination, held[b.ID], nil)
		if err != nil {
			return fmt.Errorf("backing up the binding's row %s: %w", b.ID, err)
		}
		records = append(records, infraRecoverChoiceRecord{Format: infraRecoverChoicesFormat, Choice: strandedReplaceBindingRowFlag, Store: infraRecoverChoiceStoreBinding, RecordedAt: now, Entry: entry})
	}
	for _, b := range drop {
		entry, err := infraBackupEntryFor(source, b, nil)
		if err != nil {
			return fmt.Errorf("backing up the work-store row %s: %w", b.ID, err)
		}
		records = append(records, infraRecoverChoiceRecord{Format: infraRecoverChoicesFormat, Choice: strandedDropWorkRowFlag, Store: infraRecoverChoiceStoreWork, RecordedAt: now, Entry: entry})
	}
	if err := appendInfraRecoverChoices(target, records); err != nil {
		return err
	}
	for _, record := range records {
		store := destination
		if record.Store == infraRecoverChoiceStoreWork {
			store = source
		}
		if err := removeInfraRowWithEdges(store, record.Entry); err != nil {
			return fmt.Errorf("removing %s from the %s store (it is backed up in %s): %w", record.Entry.Bead.ID, record.Store, target.RecoverChoicesBackupPath(), err)
		}
	}
	return nil
}

// removeInfraRowWithEdges removes one row and every edge touching it, as the
// backup entry records them. A store whose delete cascades its edges has
// already dropped them; one whose delete does not would otherwise keep an edge
// that now points at a different bead under the same id.
func removeInfraRowWithEdges(store beads.Store, entry infraBackupEntry) error {
	for _, edges := range [][]infraBackupEdge{entry.Deps, entry.Dependents} {
		for _, edge := range edges {
			if err := store.DepRemove(edge.IssueID, edge.DependsOnID); err != nil && !errors.Is(err, beads.ErrNotFound) {
				return fmt.Errorf("removing dep %s: %w", edge, err)
			}
		}
	}
	if err := store.Delete(entry.Bead.ID); err != nil && !errors.Is(err, beads.ErrNotFound) {
		return err
	}
	return nil
}

// readInfraRecoverChoices returns every record the backup holds, oldest first.
// An absent backup holds none.
func readInfraRecoverChoices(target infraBindingTarget) ([]infraRecoverChoiceRecord, error) {
	contents, err := os.ReadFile(target.RecoverChoicesBackupPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the recover-choices backup %s: %w", target.RecoverChoicesBackupPath(), err)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	var records []infraRecoverChoiceRecord
	for decoder.More() {
		var record infraRecoverChoiceRecord
		if err := decoder.Decode(&record); err != nil {
			return nil, fmt.Errorf("reading the recover-choices backup %s: record %d: %w", target.RecoverChoicesBackupPath(), len(records)+1, err)
		}
		if record.Format != infraRecoverChoicesFormat {
			return nil, fmt.Errorf("reading the recover-choices backup %s: record %d has format %q, want %q", target.RecoverChoicesBackupPath(), len(records)+1, record.Format, infraRecoverChoicesFormat)
		}
		records = append(records, record)
	}
	return records, nil
}

// infraRecoverChoiceIDs returns, per choice flag, the ids earlier runs acted
// on.
func infraRecoverChoiceIDs(target infraBindingTarget) (map[string]map[string]bool, error) {
	records, err := readInfraRecoverChoices(target)
	if err != nil {
		return nil, err
	}
	ids := map[string]map[string]bool{}
	for _, record := range records {
		if ids[record.Choice] == nil {
			ids[record.Choice] = map[string]bool{}
		}
		ids[record.Choice][record.Entry.Bead.ID] = true
	}
	return ids, nil
}

// appendInfraRecoverChoices adds records to the backup and proves them from
// disk. The file is rewritten whole (temp file, sync, rename) rather than
// appended in place, so a crash mid-write leaves the previous backup intact
// instead of a torn last line no later run could read.
func appendInfraRecoverChoices(target infraBindingTarget, records []infraRecoverChoiceRecord) error {
	if len(records) == 0 {
		return nil
	}
	previous, err := readInfraRecoverChoices(target)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(target.Root, 0o755); err != nil {
		return fmt.Errorf("writing the recover-choices backup: %w", err)
	}
	tmp, err := os.CreateTemp(target.Root, infraRecoverChoicesBackupName+".tmp-*")
	if err != nil {
		return fmt.Errorf("writing the recover-choices backup: %w", err)
	}
	name := tmp.Name()
	fail := func(cause error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("writing the recover-choices backup: %w", cause)
	}
	writer := bufio.NewWriter(tmp)
	encoder := json.NewEncoder(writer)
	for _, record := range append(previous, records...) {
		if err := encoder.Encode(record); err != nil {
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
		return fmt.Errorf("writing the recover-choices backup: %w", err)
	}
	if err := infraMigrationRename(name, target.RecoverChoicesBackupPath()); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("writing the recover-choices backup: %w", err)
	}
	if dir, err := os.Open(target.Root); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return verifyInfraRecoverChoices(target, len(previous), records)
}

// verifyInfraRecoverChoices proves the backup holds want, byte for byte as
// JSON, after the skip records an earlier run wrote. Nothing is removed until
// it does.
func verifyInfraRecoverChoices(target infraBindingTarget, skip int, want []infraRecoverChoiceRecord) error {
	got, err := readInfraRecoverChoices(target)
	if err != nil {
		return fmt.Errorf("proving the recover-choices backup: %w", err)
	}
	if len(got) != skip+len(want) {
		return fmt.Errorf("proving the recover-choices backup: %s holds %d record(s), want %d; nothing was removed", target.RecoverChoicesBackupPath(), len(got), skip+len(want))
	}
	for i, w := range want {
		wantJSON, err := json.Marshal(w)
		if err != nil {
			return fmt.Errorf("proving the recover-choices backup: encoding %s: %w", w.Entry.Bead.ID, err)
		}
		gotJSON, err := json.Marshal(got[skip+i])
		if err != nil {
			return fmt.Errorf("proving the recover-choices backup: encoding %s: %w", w.Entry.Bead.ID, err)
		}
		if !bytes.Equal(wantJSON, gotJSON) {
			return fmt.Errorf("proving the recover-choices backup: %s holds a different %s than the %s store; nothing was removed", target.RecoverChoicesBackupPath(), w.Entry.Bead.ID, w.Store)
		}
	}
	return nil
}

// infraHeldChoiceSummary is the one line a run that acted prints per choice.
func infraHeldChoiceSummary(target infraBindingTarget, replace, drop []beads.Bead) string {
	var parts []string
	if len(replace) > 0 {
		parts = append(parts, fmt.Sprintf("replaced the binding's row (by --%s): %s", strandedReplaceBindingRowFlag, beadIDList(replace)))
	}
	if len(drop) > 0 {
		parts = append(parts, fmt.Sprintf("removed the work-store row (by --%s): %s", strandedDropWorkRowFlag, beadIDList(drop)))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n") + fmt.Sprintf("\nevery removed row, with its edges, is backed up in %s", target.RecoverChoicesBackupPath())
}

// beadIDList joins the ids of rows.
func beadIDList(rows []beads.Bead) string {
	ids := make([]string, 0, len(rows))
	for _, b := range rows {
		ids = append(ids, b.ID)
	}
	return strings.Join(ids, ", ")
}
