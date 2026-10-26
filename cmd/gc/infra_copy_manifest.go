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
//	#gc:written 2026-08-10T12:00:00.123456789Z
//	#gc:created mc-1 2026-08-01T09:30:00Z
//	#gc:created mc-wisp-jkl 2026-08-09T18:02:11Z
//
// An older build reads every non-empty line as an id. The annotation lines
// become ids nothing can match ('#' and spaces never occur in a bead id), so a
// downgraded binary still classifies, clears and recovers exactly as it did,
// and its own superset rewrite carries the annotations through. Its only
// visible difference is an inflated "proven copy" count.
//
// A manifest with no annotations is the legacy format. Its entries carry no
// created_at, and its written time is the file's modification time — the one
// record of when the manifest was last written that the old format has (for
// either format the later of the two is used; see readInfraCopyManifestRecord).
// A legacy entry matches a row by id unless that row was created after the
// manifest was written.

const (
	infraManifestWrittenPrefix = "#gc:written "
	infraManifestCreatedPrefix = "#gc:created "
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
	// WrittenAt is when the manifest was last written (zero when unknown).
	WrittenAt time.Time
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
	// manifest that lists it.
	if m.createdAfterWrite(b) {
		return infraManifestReusedID
	}
	return infraManifestCarried
}

// Carried reports whether b is a bead the copy was proven to deliver.
func (m infraCopyManifest) Carried(b beads.Bead) bool {
	return m.Match(b) == infraManifestCarried
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

// writeInfraCopyManifest records ids with no created_at of their own. It is
// writeInfraCopyManifestRecord for a caller that holds only ids.
func writeInfraCopyManifest(target infraBindingTarget, ids []string) error {
	return writeInfraCopyManifestRecord(target, ids, nil)
}

// writeInfraCopyManifestRecord records the ids the equality stage proved the
// binding held, each with its created_at where created has one, and the time
// of this write. It is written atomically (temp + rename) and before the
// marker, so a marker never claims convergence the manifest cannot
// substantiate and a crash mid-write cannot leave a truncated manifest that
// reads as complete.
func writeInfraCopyManifestRecord(target infraBindingTarget, ids []string, created map[string]time.Time) error {
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
	lines = append(lines, infraManifestWrittenPrefix+time.Now().UTC().Format(time.RFC3339Nano))
	annotated := make([]string, 0, len(ids))
	for _, id := range ids {
		if at, ok := created[id]; ok && !at.IsZero() {
			annotated = append(annotated, infraManifestCreatedPrefix+id+" "+at.UTC().Format(time.RFC3339Nano))
		}
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
	// The later of the recorded write time and the file's modification time.
	// The legacy format records no write time, so its modification time is the
	// only record of when it was last written. And an older build that rewrites
	// an annotated manifest (recover-stranded's superset write, after a
	// downgrade) carries the old #gc:written line through verbatim while adding
	// ids it recorded no created_at for: the modification time is what keeps
	// those from reading as younger than their manifest. A stat failure leaves
	// the recorded time, or none, which only weakens the time rule.
	if info, err := os.Stat(path); err == nil && info.ModTime().After(manifest.WrittenAt) {
		manifest.WrittenAt = info.ModTime()
	}
	return manifest, true, nil
}
