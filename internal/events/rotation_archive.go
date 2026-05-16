package events

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// archiveInfo describes a parsed archive filename. Archives live next
// to the active log under the convention
// events.jsonl.archive-<UTC-ts>-seq-<firstSeq>-<lastSeq>.gz.
type archiveInfo struct {
	Basename  string    // filename without directory
	Timestamp time.Time // UTC time of rotation
	FirstSeq  uint64
	LastSeq   uint64
}

const archivePrefix = "events.jsonl.archive-"

// archiveTimestampLayout is the compact UTC-pinned timestamp embedded
// in archive filenames. Filenames must sort lexicographically in
// chronological order so directory listings remain in oldest-first
// order without requiring metadata reads.
const archiveTimestampLayout = "20060102T150405Z"

// archiveBasenameRE matches the full convention: timestamp segment,
// "seq" sentinel, two non-negative integers, and the gzip suffix.
var archiveBasenameRE = regexp.MustCompile(
	`^events\.jsonl\.archive-(\d{8}T\d{6}Z)-seq-(\d+)-(\d+)\.gz$`,
)

// legacyArchiveRE matches pre-rotation archives that predate the
// seq-stamped convention. They look like
// events.jsonl.archive-YYYYMMDD.gz with no time-of-day component.
var legacyArchiveRE = regexp.MustCompile(
	`^events\.jsonl\.archive-\d{8}\.gz$`,
)

// formatArchiveBasename returns the canonical archive basename for the
// given rotation timestamp and seq window. The timestamp is always
// rendered in UTC so filenames are comparable across hosts.
func formatArchiveBasename(ts time.Time, firstSeq, lastSeq uint64) string {
	return fmt.Sprintf(
		"%s%s-seq-%d-%d.gz",
		archivePrefix,
		ts.UTC().Format(archiveTimestampLayout),
		firstSeq,
		lastSeq,
	)
}

// formatRotatingBasename returns the in-flight rename target for a
// rotation that has captured the seq window [firstSeq, lastSeq].
// Embedding the seq range in the filename — the same technique used
// by the canonical archive name — guarantees uniqueness even when
// multiple rotations land in the same second, so os.Rename never
// races with itself on rapid back-to-back rotations.
func formatRotatingBasename(ts time.Time, firstSeq, lastSeq uint64) string {
	return fmt.Sprintf(
		"events.jsonl.rotating-%s-seq-%d-%d",
		ts.UTC().Format(archiveTimestampLayout),
		firstSeq,
		lastSeq,
	)
}

// parseArchiveBasename extracts (timestamp, firstSeq, lastSeq) from a
// filename in the canonical archive convention. Returns an error for
// any input that does not match — including legacy filenames missing
// the seq window — so callers can route those down a different path.
func parseArchiveBasename(name string) (archiveInfo, error) {
	m := archiveBasenameRE.FindStringSubmatch(name)
	if m == nil {
		return archiveInfo{}, fmt.Errorf("not a canonical events archive: %q", name)
	}
	ts, err := time.Parse(archiveTimestampLayout, m[1])
	if err != nil {
		return archiveInfo{}, fmt.Errorf("archive %q: parsing timestamp %q: %w", name, m[1], err)
	}
	first, err := strconv.ParseUint(m[2], 10, 64)
	if err != nil {
		return archiveInfo{}, fmt.Errorf("archive %q: parsing first seq: %w", name, err)
	}
	last, err := strconv.ParseUint(m[3], 10, 64)
	if err != nil {
		return archiveInfo{}, fmt.Errorf("archive %q: parsing last seq: %w", name, err)
	}
	if first > last {
		return archiveInfo{}, fmt.Errorf("archive %q: first seq %d > last seq %d", name, first, last)
	}
	return archiveInfo{
		Basename:  name,
		Timestamp: ts,
		FirstSeq:  first,
		LastSeq:   last,
	}, nil
}

// isLegacyArchiveBasename reports whether the given basename matches
// the pre-rotation archive convention (events.jsonl.archive-YYYYMMDD.gz)
// without the seq window. Legacy files exist on at least one production
// city; see B-4 for the migration sweep.
func isLegacyArchiveBasename(name string) bool {
	return legacyArchiveRE.MatchString(name)
}

// archiveOverlapsFilter reports whether the archive's seq range can
// possibly contain events that satisfy filter. The skip-fast read path
// uses this to avoid gunzipping archives whose entire window has
// already been excluded by the caller's AfterSeq predicate.
func archiveOverlapsFilter(info archiveInfo, filter Filter) bool {
	if filter.AfterSeq > 0 && info.LastSeq <= filter.AfterSeq {
		return false
	}
	return true
}

// hasRotatingPrefix reports whether the basename matches the
// in-flight rename target events.jsonl.rotating-*. Used by the orphan
// reaper.
func hasRotatingPrefix(name string) bool {
	return strings.HasPrefix(name, "events.jsonl.rotating-")
}

// hasGzipTmpSuffix reports whether the basename matches the in-flight
// gzip output .gz.tmp suffix. Used by the orphan reaper.
func hasGzipTmpSuffix(name string) bool {
	return strings.HasSuffix(name, ".gz.tmp")
}
