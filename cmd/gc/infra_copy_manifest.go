package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
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
// only when both match. And a row created after the manifest was written can
// never be one the copy carried, whatever its id: it counts as stranded — never
// as retained, never as removed since cutover.
//
// # The format, and why it is shaped like this
//
// The file keeps the legacy shape — one bare id per line — and carries the new
// facts on annotation lines a reader of either generation can live with:
//
//	mc-1
//	mc-wisp-jkl
//	mc-old
//	#gc:written 2026-10-26T12:00:00.123456789Z
//	#gc:legacy-written 2026-08-10T12:00:01Z
//	#gc:created mc-1 2026-08-01T09:30:00Z
//	#gc:created mc-wisp-jkl 2026-08-09T18:02:11Z
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
// #gc:created line (mc-old above) is a legacy entry. A legacy entry matches a
// row by id, unless the row was created after the LEGACY BOUND: the time the
// legacy manifest was written. That bound is never the file's modification
// time. An mtime moves whenever the file is copied back from its .bak, synced
// without -t, or touched, and a bound that moves later re-opens bare-id
// matching for every id reused in between — and the clear deletes on it.
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
//     bound, so its rows read as reused: they are named stranded, never
//     deleted, and this build's recover-stranded proves them against the
//     binding and records them with their created_at.
//   - The first rewrite by this build persists that bound as #gc:legacy-
//     written, and every later rewrite carries it unchanged. #gc:written,
//     which every rewrite moves forward, never judges a legacy entry.
//   - With no trustworthy bound (a legacy manifest whose marker carries no
//     readable time; an annotated manifest with legacy entries and no
//     #gc:legacy-written line) a legacy entry is UNBOUNDED. Read-only verdicts
//     keep the id-only reading they always had; the clear refuses to delete
//     such a row; recover-stranded resolves them: one the binding still holds
//     is proven field-equal and recorded with its created_at; one it no
//     longer holds is moved and recorded when the work row is closed, and
//     needs the operator's per-id --move or --record-carried when it is open
//     (the frozen row would otherwise come back OPEN, #5987).

const (
	infraManifestWrittenPrefix       = "#gc:written "
	infraManifestLegacyWrittenPrefix = "#gc:legacy-written "
	infraManifestCreatedPrefix       = "#gc:created "
)

// infraManifestClockSlack absorbs timestamp granularity: the work store
// persists created_at at second granularity (truncated, or rounded by some
// backends), and the written time is taken from the local clock. A row created
// within this window after the written time is not called stranded on the
// time rule alone; the (id, created_at) key still decides it. A var so a test
// can pin the rule without sleeping across a second boundary.
var infraManifestClockSlack = time.Second

// infraCopyManifest is one read of the copy manifest.
type infraCopyManifest struct {
	// IDs is the set of ids the copy was proven to deliver.
	IDs map[string]bool
	// Created is each id's created_at when the manifest recorded one. An id
	// with no entry is a legacy entry.
	Created map[string]time.Time
	// WrittenAt is when the manifest was last written, as the manifest itself
	// records it (zero when unknown). It judges rows the manifest does not
	// list. For a legacy manifest it is the legacy bound.
	WrittenAt time.Time
	// LegacyBound is when the legacy part of the manifest was written: the
	// bound a legacy entry (one with no created_at) is judged by. Zero means
	// no trustworthy bound is known, and such an entry is unbounded.
	LegacyBound time.Time
}

// infraManifestMatch is how one work-store row relates to the manifest.
type infraManifestMatch int

const (
	// infraManifestUnlisted: the row's id is not in the manifest.
	infraManifestUnlisted infraManifestMatch = iota
	// infraManifestCarried: the row is the bead the copy carried.
	infraManifestCarried
	// infraManifestReusedID: the manifest lists the row's id, but for a
	// different bead — the id was reused after the copy. The row was never
	// carried.
	infraManifestReusedID
	// infraManifestLegacyUnbounded: a legacy entry lists the row's id and no
	// trustworthy bound says whether the row is younger than the entry.
	// Read-only verdicts read it as carried, as they always did; nothing
	// deletes it on that reading.
	infraManifestLegacyUnbounded
)

// Len is the number of ids the manifest records.
func (m infraCopyManifest) Len() int { return len(m.IDs) }

// createdAfterWrite reports whether b was created after the manifest was
// last written, beyond the clock slack. Unknown times answer false.
func (m infraCopyManifest) createdAfterWrite(b beads.Bead) bool {
	if m.WrittenAt.IsZero() || b.CreatedAt.IsZero() {
		return false
	}
	return b.CreatedAt.After(m.WrittenAt.Add(infraManifestClockSlack))
}

// Match classifies one work-store row against the manifest.
func (m infraCopyManifest) Match(b beads.Bead) infraManifestMatch {
	if !m.IDs[b.ID] {
		return infraManifestUnlisted
	}
	if recorded, ok := m.Created[b.ID]; ok && !recorded.IsZero() && !b.CreatedAt.IsZero() {
		if sameCreatedSecond(recorded, b.CreatedAt) {
			return infraManifestCarried
		}
		return infraManifestReusedID
	}
	// A legacy entry: the id alone, unless the row is younger than the
	// legacy bound. Never judged by WrittenAt, which every rewrite moves.
	if m.LegacyBound.IsZero() {
		return infraManifestLegacyUnbounded
	}
	if !b.CreatedAt.IsZero() && b.CreatedAt.After(m.LegacyBound.Add(infraManifestClockSlack)) {
		return infraManifestReusedID
	}
	return infraManifestCarried
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

// infraManifestCreatedOf collects the created_at of every row in rows whose id
// is in ids, for a manifest write.
func infraManifestCreatedOf(rows []beads.Bead, ids []string) map[string]time.Time {
	want := setOf(ids)
	created := make(map[string]time.Time, len(ids))
	for _, b := range rows {
		if want[b.ID] && !b.CreatedAt.IsZero() {
			created[b.ID] = b.CreatedAt
		}
	}
	return created
}

// writeInfraCopyManifest records ids with no created_at of their own and no
// legacy bound. It is writeInfraCopyManifestRecord for a caller that holds
// only ids (genesis writes none).
func writeInfraCopyManifest(target infraBindingTarget, ids []string) error {
	return writeInfraCopyManifestRecord(target, ids, nil, time.Time{})
}

// writeInfraCopyManifestRecord records the ids the equality stage proved the
// binding held, each with its created_at where created has one, and the time
// of this write. It is written atomically (temp + rename) and before the
// marker, so a marker never claims convergence the manifest cannot
// substantiate and a crash mid-write cannot leave a truncated manifest that
// reads as complete.
//
// legacyBound is persisted for the entries with no created_at (see "Legacy
// entries" above). A rewrite must pass the bound it read, unchanged: it is the
// one time in the file that a rewrite does not move.
func writeInfraCopyManifestRecord(target infraBindingTarget, ids []string, created map[string]time.Time, legacyBound time.Time) error {
	if err := os.MkdirAll(target.Root, 0o755); err != nil {
		return fmt.Errorf("writing the infra copy manifest: %w", err)
	}
	tmp, err := os.CreateTemp(target.Root, infraCopyManifestName+".tmp-*")
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
	writtenAt := time.Now().UTC()
	lines = append(lines, infraManifestWrittenPrefix+writtenAt.Format(time.RFC3339Nano))
	annotated := make([]string, 0, len(ids))
	legacyEntries := false
	for _, id := range ids {
		if at, ok := created[id]; ok && !at.IsZero() {
			annotated = append(annotated, infraManifestCreatedPrefix+id+" "+at.UTC().Format(time.RFC3339Nano))
		} else {
			legacyEntries = true
		}
	}
	// Always bound the bare ids, so none is ever unbounded by accident. With
	// legacy entries in the file the bound is theirs, unchanged (or none when
	// it is unknown: they stay unbounded rather than gain a later bound). With
	// none, it is this write's time: a bare id an OLDER build appends later
	// (its recover-stranded superset rewrite carries these lines through and
	// adds ids without created_at) is then judged against when this manifest
	// was written — a row younger than that reads as reused and is named
	// stranded, which this build's recover-stranded resolves by proving and
	// recording it, instead of reading as unbounded.
	switch {
	case legacyEntries && !legacyBound.IsZero():
		lines = append(lines, infraManifestLegacyWrittenPrefix+legacyBound.UTC().Format(time.RFC3339Nano))
	case !legacyEntries:
		lines = append(lines, infraManifestLegacyWrittenPrefix+writtenAt.Format(time.RFC3339Nano))
	}
	sort.Strings(annotated)
	lines = append(lines, annotated...)
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
	if err := infraMigrationRename(name, target.ManifestPath()); err != nil {
		return cleanup(err)
	}
	return nil
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
	path := target.ManifestPath()
	contents, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return infraCopyManifest{}, false, nil
		}
		return infraCopyManifest{}, false, fmt.Errorf("reading the infra copy manifest %s: %w", path, err)
	}
	manifest := infraCopyManifest{IDs: make(map[string]bool), Created: make(map[string]time.Time)}
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
			manifest.Created[id] = at
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
	// carried #gc:legacy-written bound and read as reused when younger, which
	// names them stranded rather than deleting anything.
	if !annotated {
		if bound, ok := readInfraMarkerBound(target); ok {
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
	contents, err := os.ReadFile(target.MarkerPath())
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
