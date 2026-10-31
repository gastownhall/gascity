package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// The copy manifest (infra.migrated.beads) records which work-store beads the
// equality stage proved the binding received. Every later verdict — the boot
// containment check, `gc storage status`, the clear, recover-stranded — tests a
// work-store row against it.
//
// # Why a bare id is not enough
//
// bd reuses short ids. A wisp id like mc-wisp-jkl has a three-character suffix,
// so once the original row is gone from the work store the same id is minted
// again for an unrelated bead. On maintainer-city mc-wisp-jkl was an
// auto-handoff mail created 2026-08-15, five days after the 2026-08-10 manifest
// that already listed mc-wisp-jkl. Matched by id alone it read as "removed
// since cutover / retained", so the check called a never-copied bead safe and
// the clear would have deleted it.
//
// So an entry is keyed by (id, created_at): a row is the bead the copy carried
// only when both match, and a row with no created_at of its own never matches
// an entry that records one. A bead that carries no created_at is recorded as
// such (#gc:no-created-at), and that entry matches only a row with none: every
// store's create stamps one, so no bead minted later under the id can carry
// none. Any other row whose id the binding holds is judged by the binding's
// row under that id (infraHeldRowIsThisBead), whatever its age: that very bead
// makes it a second copy of a bead the binding owns rather than a strand, and
// another bead makes it a strand only the operator's per-id choice resolves. A
// row the binding does not hold, which the manifest does not record, is
// stranded.
//
// # The format, and why it is shaped like this
//
// The file keeps the legacy shape — one bare id per line — and carries the new
// facts on annotation lines a reader of either generation can live with:
//
//	mc-1
//	mc-2
//	mc-wisp-jkl
//	mc-old
//	#gc:written 2026-10-26T12:00:00.123456789Z
//	#gc:legacy-written 2026-08-10T12:00:01Z
//	#gc:classifier-generation 2
//	#gc:created mc-1 2026-08-01T09:30:00Z
//	#gc:created mc-wisp-jkl 2026-08-09T18:02:11Z
//	#gc:no-created-at mc-2
//
// An older build reads every non-empty line as an id. The annotation lines
// become ids nothing can match ('#' and spaces never occur in a bead id), so a
// downgraded binary still classifies, clears and recovers exactly as it did,
// and its own superset rewrite carries the annotations through. Its only
// visible difference is an inflated "proven copy" count.
//
// # Legacy entries, and the bound that judges them
//
// A manifest with no annotations is the legacy format, and an entry with no
// #gc:created or #gc:no-created-at line (mc-old above) is a legacy entry. A
// legacy entry matches a row by id, unless the row was created after the
// LEGACY BOUND: the time the legacy manifest was written. That bound is never
// the file's modification time. An mtime moves whenever the file is copied
// back from its .bak, synced without -t, or touched, and a bound that moves
// later re-opens bare-id matching for every id reused in between — and the
// clear deletes on it.
//
//   - The bound is an UPPER bound on when a legacy entry's bead was copied,
//     never a lower one: a row created after it was certainly not copied,
//     while a reused id minted between the copy and the bound still reads as
//     carried. It is as tight as the evidence allows; on maintainer-city the
//     marker may record a time up to ~2.5 h after the copy, and no row in its
//     work store falls in that window today.
//   - A legacy manifest (no #gc:written) takes its bound from the convergence
//     marker beside it: the migration writes the manifest and then the marker,
//     and the marker's CONTENT carries the time to the second (every build
//     since the marker was introduced, 2026-08-07, writes it). A later
//     manifest extension by an older recover-stranded is younger than that
//     bound, so its rows read as PRESUMED reused and are never deleted on the
//     id alone. While the binding holds such a row, the binding's row under
//     the id decides it, and this build's recover-stranded proves it against
//     that row and records it with its created_at. Once the binding has
//     collected it, nothing tells a row an older recover carried from a later
//     bead that reused the id, so recover-stranded treats it as it treats an
//     unbounded entry (below).
//   - The first rewrite by this build persists that bound as #gc:legacy-
//     written, and every later rewrite carries it unchanged. #gc:written
//     never judges a legacy entry.
//   - A copy by this build leaves no legacy entry: it records a row that
//     carries no created_at with #gc:no-created-at.
//   - With no trustworthy bound (a legacy manifest whose marker carries no
//     readable time; an annotated manifest with legacy entries and no
//     #gc:legacy-written line) a legacy entry is UNBOUNDED. So is a legacy
//     entry against a row with no created_at, which no bound can place, and
//     an entry that records a created_at, against a row that carries none:
//     nothing ties the row to the bead the entry names. The binding's row
//     under the id decides one the binding still holds, as for any row the
//     manifest does not record as carried; one it no longer holds keeps the
//     id-only reading it always had (retained), named unrecorded. The clear
//     refuses to delete either. recover-stranded resolves them: one the
//     binding still holds is proven the same bead and recorded with its
//     created_at (with #gc:no-created-at when it has none); one it no longer
//     holds is moved and recorded when the work row is closed, and needs the
//     operator's per-id --move or --record-carried when it is open (the
//     frozen row would otherwise come back OPEN, #5987).
//
// # The write time
//
// #gc:written is when the copy wrote the manifest. A write that leaves no
// legacy entry persists it as #gc:legacy-written too, so it bounds the bare
// ids an older build appends later: a row younger than it reads as presumed
// reused, never as the bead the entry names. A rewrite carries it unchanged,
// as it carries the legacy bound and the classifier generation, because only a
// copy says when the copy was made. Moved forward, it would read an id the
// work store minted again after the copy as that copy's bead, and the clear
// would delete the row. It judges no row the manifest does not list: such a
// row is stranded unless the binding holds a row under its id, which then
// decides, whatever the row's age.
//
// # The classifier generation
//
// The copy carried the rows its classifier put outside the work class, so a
// manifest only says anything about those. A later build that classifies more
// shapes as infrastructure (coordclass.Generation) would otherwise read every
// such row an earlier copy deliberately left in the work store — a
// startup-health episode, an own-session wait — as never carried, and refuse
// the city over a strand no writer made. So the manifest records the
// generation its copy classified with (#gc:classifier-generation), and every
// verdict judged against it — the containment check, `gc storage status`, the
// clear, recover-stranded — selects work-store rows with that generation
// (readInfraSnapshotAt). A manifest with no such line was written before
// generations existed and reads as coordclass.GenerationLegacy. A rewrite
// carries the generation unchanged; only a copy records the current one, and
// a re-copy from the clear's backup keeps the generation of the manifest it
// replaces, because the backup holds exactly the rows that generation selected.
// The rows a widening reclassifies stay where the earlier copy left them,
// unchecked, exactly as that build left them.

const (
	infraManifestWrittenPrefix       = "#gc:written "
	infraManifestLegacyWrittenPrefix = "#gc:legacy-written "
	infraManifestCreatedPrefix       = "#gc:created "
	infraManifestNoCreatedPrefix     = "#gc:no-created-at "
	infraManifestGenerationPrefix    = "#gc:classifier-generation "
	// The rig importer's annotations (rig_import.go). A city manifest carries
	// none of them, and a build that does not know them reads each as an
	// annotation, never an id.
	infraManifestScopePrefix          = "#gc:scope "
	infraManifestScopeAnnotatedPrefix = "#gc:scope-annotated "
	infraManifestGatePrefix           = "#gc:gate "
)

// infraManifestClockSlack absorbs timestamp granularity: the work store
// persists created_at at second granularity (truncated, or rounded by some
// backends), and the legacy bound is taken from the local clock. A row created
// within this window after the bound does not read as a reused id on the time
// rule alone. A var so a test can pin the rule without sleeping across a
// second boundary.
var infraManifestClockSlack = time.Second

// infraCopyManifest is one read of the copy manifest.
type infraCopyManifest struct {
	// IDs is the set of ids the copy was proven to deliver.
	IDs map[string]bool
	// Created is each id's created_at when the manifest recorded one. A zero
	// time records that the bead carries none (#gc:no-created-at). An id with
	// no entry is a legacy entry.
	Created map[string]time.Time
	// WrittenAt is when the copy wrote the manifest, as the manifest itself
	// records it (zero when unknown); a rewrite carries it (see "The write
	// time"). For a legacy manifest it is the legacy bound.
	WrittenAt time.Time
	// LegacyBound is when the legacy part of the manifest was written: the
	// bound a legacy entry (one with no created_at) is judged by. Zero means
	// no trustworthy bound is known, and such an entry is unbounded.
	LegacyBound time.Time
	// Generation is the classifier generation the copy selected its rows
	// with; work-store rows are judged against this manifest under it (see
	// "The classifier generation").
	Generation coordclass.Generation
	// Scope is the scope a rig manifest records (#gc:scope rig:<name>); empty
	// on the city's manifest.
	Scope string
	// ScopeAnnotated names the ids the rig importer stamped with
	// gc.root_store_ref because the rig row carried none (#gc:scope-annotated),
	// so a reverse can restore the row's byte shape.
	ScopeAnnotated map[string]bool
	// Gates names the cross-store gates a rig import minted in the binding
	// (#gc:gate). They are not rig rows: no containment verdict reads them.
	Gates map[string]bool
}

// infraManifestMatch is how one work-store row relates to the manifest.
type infraManifestMatch int

const (
	// infraManifestUnlisted: the row's id is not in the manifest.
	infraManifestUnlisted infraManifestMatch = iota
	// infraManifestCarried: the row is the bead the copy carried.
	infraManifestCarried
	// infraManifestReusedID: the manifest lists the row's id, but for a
	// different bead — its entry records the created_at of the bead the copy
	// carried, or that it carried none, and the row does not share it: the id
	// was reused after the copy. The row was never carried.
	infraManifestReusedID
	// infraManifestPresumedReused: a legacy entry lists the row's id, and the
	// row was created after the legacy bound, so it reads as reused: stranded,
	// and never deleted on the id alone. Only presumed, though: an older
	// build's recover-stranded appended the bare id of every row it carried,
	// so the entry may name this very bead, whose copy the binding has since
	// closed and collected. Moving the row back would revive it (#5987).
	infraManifestPresumedReused
	// infraManifestLegacyUnbounded: the manifest lists the row's id and no
	// time says whether the row is the bead it lists: a legacy entry no
	// trustworthy bound judges, a legacy entry against a row with no
	// created_at for the bound to place, or an entry that records a created_at
	// the row carries none of. The id-only reading (ReadsCarried) takes it as
	// carried, as it always did; nothing deletes it on that reading.
	infraManifestLegacyUnbounded
)

// Len is the number of ids the manifest records.
func (m infraCopyManifest) Len() int { return len(m.IDs) }

// Match classifies one work-store row against the manifest.
func (m infraCopyManifest) Match(b beads.Bead) infraManifestMatch {
	if !m.IDs[b.ID] {
		return infraManifestUnlisted
	}
	if recorded, ok := m.Created[b.ID]; ok {
		switch {
		case recorded.IsZero() && b.CreatedAt.IsZero():
			// The copy carried a bead with no created_at under the id, and no
			// bead minted later can carry none: this is that bead.
			return infraManifestCarried
		case recorded.IsZero():
			return infraManifestReusedID
		case b.CreatedAt.IsZero():
			// Keyed by a time the row carries none of, so nothing ties it to
			// the bead the copy carried; the legacy bound below would read it
			// as carried on its id alone.
			return infraManifestLegacyUnbounded
		case sameCreatedSecond(recorded, b.CreatedAt):
			return infraManifestCarried
		default:
			return infraManifestReusedID
		}
	}
	// A legacy entry: the id alone, judged by the legacy bound and never by
	// WrittenAt. A row with no created_at is one the bound cannot place.
	switch {
	case m.LegacyBound.IsZero(), b.CreatedAt.IsZero():
		return infraManifestLegacyUnbounded
	case b.CreatedAt.After(m.LegacyBound.Add(infraManifestClockSlack)):
		return infraManifestPresumedReused
	default:
		return infraManifestCarried
	}
}

// Carried reports whether b is a bead the copy was proven to deliver. An
// unbounded legacy entry is NOT carried here: this is the reading a deletion
// may act on.
func (m infraCopyManifest) Carried(b beads.Bead) bool {
	return m.Match(b) == infraManifestCarried
}

// ReadsCarried is the read-only reading: Carried, plus an unbounded legacy
// entry, which keeps the id-only verdict it had before entries were keyed by
// created_at. It must never license a deletion.
func (m infraCopyManifest) ReadsCarried(b beads.Bead) bool {
	switch m.Match(b) {
	case infraManifestCarried, infraManifestLegacyUnbounded:
		return true
	default:
		return false
	}
}

// sameCreatedSecond compares two created_at values at the work store's
// second granularity.
func sameCreatedSecond(a, b time.Time) bool {
	return a.UTC().Truncate(time.Second).Equal(b.UTC().Truncate(time.Second))
}

// infraManifestRecordCreated is the created_at map a manifest write records
// once the rows of rows whose ids are in ids are proven: previous (nil for a
// copy), with each such row recorded under its own created_at, a zero one when
// it carries none (#gc:no-created-at, which Match reads as carried whatever
// the legacy bound). That replaces whatever previous held for the id — when a
// recovered row reused an id, the bead the binding now holds under it is the
// recovered one.
func infraManifestRecordCreated(previous map[string]time.Time, rows []beads.Bead, ids []string) map[string]time.Time {
	recorded := make(map[string]time.Time, len(previous)+len(ids))
	for id, at := range previous {
		recorded[id] = at
	}
	want := setOf(ids)
	for _, b := range rows {
		if want[b.ID] {
			recorded[b.ID] = b.CreatedAt
		}
	}
	return recorded
}

// writeInfraCopyManifest records ids by id alone, as a copy, under the
// current classifier generation. It is writeInfraCopyManifestRecord for a
// caller that holds only ids (genesis writes none).
func writeInfraCopyManifest(target infraBindingTarget, ids []string) error {
	return writeInfraCopyManifestRecord(target, ids, nil, coordclass.CurrentGeneration)
}

// writeInfraCopyManifestRecord records what a copy proved: the ids the
// equality stage proved the binding held, each with its created_at where
// created records one, at this write. The write's time is #gc:written and the
// bound of every id created does not record (see "Legacy entries" above).
// generation is the classifier generation the copy selected its rows with
// (see "The classifier generation").
func writeInfraCopyManifestRecord(target infraBindingTarget, ids []string, created map[string]time.Time, generation coordclass.Generation) error {
	now := time.Now().UTC()
	return writeInfraCopyManifestFile(target, ids, created, now, now, generation)
}

// rewriteInfraCopyManifest rewrites the manifest previous was read from as
// ids, each with its created_at where created records one. Everything else
// previous records passes through unchanged — the write time (see "The write
// time"), the legacy bound and the classifier generation — because a rewrite
// records rows it proved, while only a copy says when, and with which
// classifier, the copy was made. A write time previous does not know (a legacy
// manifest whose marker records none) becomes this write's.
func rewriteInfraCopyManifest(target infraBindingTarget, previous infraCopyManifest, ids []string, created map[string]time.Time) error {
	writtenAt := previous.WrittenAt
	if writtenAt.IsZero() {
		writtenAt = time.Now().UTC()
	}
	return writeInfraCopyManifestFile(target, ids, created, writtenAt, previous.LegacyBound, previous.Generation)
}

// writeInfraCopyManifestFile writes the manifest. It is written atomically
// (temp + rename) and before the marker, so a marker never claims convergence
// the manifest cannot substantiate and a crash mid-write cannot leave a
// truncated manifest that reads as complete. created records each id's
// created_at, a zero one for a bead that carries none (#gc:no-created-at).
// legacyBound is persisted for the ids it does not record, the legacy
// entries; zero means none is known, and they are unbounded.
func writeInfraCopyManifestFile(target infraBindingTarget, ids []string, created map[string]time.Time, writtenAt, legacyBound time.Time, generation coordclass.Generation) error {
	return writeInfraCopyManifestFileAt(target.Root, target.ManifestPath(), ids, created, writtenAt, legacyBound, generation, nil)
}

// writeInfraCopyManifestFileAt is writeInfraCopyManifestFile for a manifest
// at path in dir: the city's, or a rig's (rigImportLocation.ManifestPath).
// extra are annotation lines appended after the created_at lines; each must
// start with '#'.
func writeInfraCopyManifestFileAt(dir, path string, ids []string, created map[string]time.Time, writtenAt, legacyBound time.Time, generation coordclass.Generation, extra []string) error {
	for _, line := range extra {
		if !strings.HasPrefix(line, "#") {
			return fmt.Errorf("writing the infra copy manifest: annotation %q does not start with '#'", line)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("writing the infra copy manifest: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("writing the infra copy manifest: %w", err)
	}
	name := tmp.Name()
	cleanup := func(cause error) error {
		_ = os.Remove(name)
		return fmt.Errorf("writing the infra copy manifest: %w", cause)
	}
	writer := bufio.NewWriter(tmp)
	write := func(line string) error {
		_, err := fmt.Fprintln(writer, line)
		return err
	}
	lines := make([]string, 0, 2*len(ids)+1)
	lines = append(lines, ids...)
	writtenAt = writtenAt.UTC()
	lines = append(lines, infraManifestWrittenPrefix+writtenAt.Format(time.RFC3339Nano))
	annotated := make([]string, 0, len(ids))
	legacyEntries := false
	for _, id := range ids {
		at, ok := created[id]
		switch {
		case !ok:
			legacyEntries = true
		case at.IsZero():
			annotated = append(annotated, infraManifestNoCreatedPrefix+id)
		default:
			annotated = append(annotated, infraManifestCreatedPrefix+id+" "+at.UTC().Format(time.RFC3339Nano))
		}
	}
	// Always bound the bare ids, so none is ever unbounded by accident. With
	// legacy entries in the file the bound is theirs, unchanged (or none when
	// it is unknown: they stay unbounded rather than gain a later bound). With
	// none, it is the manifest's write time: a bare id an OLDER build appends
	// later (its recover-stranded superset rewrite carries these lines through
	// and adds ids without created_at) is then judged against when the copy
	// wrote this manifest — a row younger than that reads as reused and is
	// named stranded, which this build's recover-stranded resolves by proving
	// and recording it, instead of reading as unbounded.
	switch {
	case legacyEntries && !legacyBound.IsZero():
		lines = append(lines, infraManifestLegacyWrittenPrefix+legacyBound.UTC().Format(time.RFC3339Nano))
	case !legacyEntries:
		lines = append(lines, infraManifestLegacyWrittenPrefix+writtenAt.Format(time.RFC3339Nano))
	}
	lines = append(lines, infraManifestGenerationPrefix+strconv.Itoa(int(generation)))
	sort.Strings(annotated)
	lines = append(lines, annotated...)
	lines = append(lines, extra...)
	for _, line := range lines {
		if err := write(line); err != nil {
			_ = tmp.Close()
			return cleanup(err)
		}
	}
	if err := writer.Flush(); err != nil {
		_ = tmp.Close()
		return cleanup(err)
	}
	// Synced before the rename: the clear deletes work-store rows on the
	// manifest's word, so it must be durable before anything relies on it.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		return cleanup(err)
	}
	if err := infraMigrationRename(name, path); err != nil {
		return cleanup(err)
	}
	syncInfraDir(dir)
	return nil
}

// syncInfraDir fsyncs a directory after a rename into it, best effort, so the
// rename itself is durable.
func syncInfraDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// readInfraCopyManifest returns the id set the copy was proven to deliver.
// recorded=false with no error means the city converged before the manifest
// existed, which is the one state in which absence cannot be classified at all.
// A verdict about a ROW must use readInfraCopyManifestRecord: an id alone
// cannot tell a carried bead from a later one that reused its id.
func readInfraCopyManifest(target infraBindingTarget) (proven map[string]bool, recorded bool, err error) {
	manifest, recorded, err := readInfraCopyManifestRecord(target)
	return manifest.IDs, recorded, err
}

// readInfraCopyManifestRecord reads the manifest in either format.
func readInfraCopyManifestRecord(target infraBindingTarget) (infraCopyManifest, bool, error) {
	return readInfraCopyManifestRecordAt(target.ManifestPath(), target.MarkerPath())
}

// readInfraCopyManifestRecordAt reads the manifest at path, in either format.
// markerPath is the convergence marker a legacy manifest takes its bound from.
func readInfraCopyManifestRecordAt(path, markerPath string) (infraCopyManifest, bool, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return infraCopyManifest{}, false, nil
		}
		return infraCopyManifest{}, false, fmt.Errorf("reading the infra copy manifest %s: %w", path, err)
	}
	manifest := infraCopyManifest{IDs: make(map[string]bool), Created: make(map[string]time.Time), Generation: coordclass.GenerationLegacy}
	annotated := false
	for _, raw := range strings.Split(string(contents), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
		case strings.HasPrefix(line, infraManifestWrittenPrefix):
			at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(strings.TrimPrefix(line, infraManifestWrittenPrefix)))
			if err != nil {
				return infraCopyManifest{}, false, fmt.Errorf("reading the infra copy manifest %s: malformed %q line: %w", path, line, err)
			}
			manifest.WrittenAt = at
			annotated = true
		case strings.HasPrefix(line, infraManifestLegacyWrittenPrefix):
			at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(strings.TrimPrefix(line, infraManifestLegacyWrittenPrefix)))
			if err != nil {
				return infraCopyManifest{}, false, fmt.Errorf("reading the infra copy manifest %s: malformed %q line: %w", path, line, err)
			}
			manifest.LegacyBound = at
		case strings.HasPrefix(line, infraManifestCreatedPrefix):
			id, stamp, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, infraManifestCreatedPrefix)), " ")
			if !ok {
				return infraCopyManifest{}, false, fmt.Errorf("reading the infra copy manifest %s: malformed %q line", path, line)
			}
			at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(stamp))
			if err != nil {
				return infraCopyManifest{}, false, fmt.Errorf("reading the infra copy manifest %s: malformed %q line: %w", path, line, err)
			}
			if at.IsZero() {
				// A zero time records no created_at, which only its own line says.
				return infraCopyManifest{}, false, fmt.Errorf("reading the infra copy manifest %s: malformed %q line: a zero time", path, line)
			}
			manifest.Created[id] = at
		case strings.HasPrefix(line, infraManifestNoCreatedPrefix):
			fields := strings.Fields(strings.TrimPrefix(line, infraManifestNoCreatedPrefix))
			if len(fields) != 1 {
				return infraCopyManifest{}, false, fmt.Errorf("reading the infra copy manifest %s: malformed %q line", path, line)
			}
			manifest.Created[fields[0]] = time.Time{}
		case strings.HasPrefix(line, infraManifestGenerationPrefix):
			n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, infraManifestGenerationPrefix)))
			if err != nil || coordclass.Generation(n) < coordclass.GenerationLegacy {
				return infraCopyManifest{}, false, fmt.Errorf("reading the infra copy manifest %s: malformed %q line", path, line)
			}
			manifest.Generation = coordclass.Generation(n)
		case strings.HasPrefix(line, infraManifestScopeAnnotatedPrefix):
			fields := strings.Fields(strings.TrimPrefix(line, infraManifestScopeAnnotatedPrefix))
			if len(fields) != 1 {
				return infraCopyManifest{}, false, fmt.Errorf("reading the infra copy manifest %s: malformed %q line", path, line)
			}
			if manifest.ScopeAnnotated == nil {
				manifest.ScopeAnnotated = map[string]bool{}
			}
			manifest.ScopeAnnotated[fields[0]] = true
		case strings.HasPrefix(line, infraManifestGatePrefix):
			fields := strings.Fields(strings.TrimPrefix(line, infraManifestGatePrefix))
			if len(fields) != 1 {
				return infraCopyManifest{}, false, fmt.Errorf("reading the infra copy manifest %s: malformed %q line", path, line)
			}
			if manifest.Gates == nil {
				manifest.Gates = map[string]bool{}
			}
			manifest.Gates[fields[0]] = true
		case strings.HasPrefix(line, infraManifestScopePrefix):
			manifest.Scope = strings.TrimSpace(strings.TrimPrefix(line, infraManifestScopePrefix))
		case strings.HasPrefix(line, "#"):
			// An annotation this build does not know: never an id.
		default:
			manifest.IDs[line] = true
		}
	}
	// The file's modification time is never read: see "Legacy entries". A
	// legacy manifest's bound is the marker's recorded time; an annotated one
	// carries its own (or none, and its legacy entries are unbounded). An older
	// build that rewrites an annotated manifest carries every annotation line
	// through as an "id" and adds bare ids of its own; those are judged by the
	// carried #gc:legacy-written bound and read as presumed reused when
	// younger, which names them stranded rather than deleting anything.
	if !annotated {
		if bound, ok := readInfraMarkerBoundAt(markerPath); ok {
			manifest.LegacyBound = bound
			manifest.WrittenAt = bound
		}
	}
	return manifest, true, nil
}

// readInfraMarkerBound returns the end of the second the convergence marker
// records as its write time: the marker's content ends with that time
// (writeInfraMigratedMarker, RFC 3339 to the second), and the manifest of the
// same run was written just before it. ok=false when the marker is absent,
// unreadable or carries no time.
func readInfraMarkerBound(target infraBindingTarget) (time.Time, bool) {
	return readInfraMarkerBoundAt(target.MarkerPath())
}

// readInfraMarkerBoundAt is readInfraMarkerBound for the marker at path.
func readInfraMarkerBoundAt(path string) (time.Time, bool) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}
	fields := strings.Fields(string(contents))
	if len(fields) == 0 {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, fields[len(fields)-1])
	if err != nil {
		return time.Time{}, false
	}
	return at.Truncate(time.Second).Add(time.Second), true
}
