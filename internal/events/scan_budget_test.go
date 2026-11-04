package events

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// seedThreeArchives builds three canonical .gz archives with disjoint seq
// windows [1,2], [3,4], [5,6] (oldest to newest) and returns the directory
// and the per-archive on-disk (compressed) sizes in the same oldest-to-newest
// order, so callers can derive exact budget boundaries from real sizes
// instead of guessed magic numbers.
func seedThreeArchives(t *testing.T) (dir string, sizes [3]int64) {
	t.Helper()
	dir = t.TempDir()
	var stderr bytes.Buffer
	windows := [3][2]uint64{{1, 2}, {3, 4}, {5, 6}}
	stamps := []time.Time{
		time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 5, 7, 12, 5, 0, 0, time.UTC),
		time.Date(2026, 5, 7, 12, 10, 0, 0, time.UTC),
	}
	for i, w := range windows {
		src := filepath.Join(dir, "src.jsonl")
		writeJSONLEvents(t, src, w[0], w[1])
		dest := filepath.Join(dir, formatArchiveBasename(stamps[i], w[0], w[1]))
		if err := gzipAndArchive(src, dest, &stderr); err != nil {
			t.Fatalf("gzip archive %d: %v", i, err)
		}
		st, err := os.Stat(dest)
		if err != nil {
			t.Fatalf("stat archive %d: %v", i, err)
		}
		sizes[i] = st.Size()
	}
	return dir, sizes
}

func TestReadNewestBoundedMatchesAscendingScanWithGenerousBudget(t *testing.T) {
	dir, _ := seedThreeArchives(t)
	path := filepath.Join(dir, "events.jsonl")
	ctx := context.Background()

	got, truncated, reachedSeq, err := readNewestBounded(ctx, path, Filter{}, 10, 1<<30)
	if err != nil {
		t.Fatalf("readNewestBounded: %v", err)
	}
	if truncated {
		t.Errorf("truncated = true, want false (generous budget covers all archives)")
	}
	if reachedSeq != 0 {
		t.Errorf("reachedSeq = %d, want 0 (not truncated)", reachedSeq)
	}

	want, err := ReadFiltered(path, Filter{})
	if err != nil {
		t.Fatalf("ReadFiltered: %v", err)
	}
	if gotSeqs, wantSeqs := seqsOf(got), seqsOf(want); !reflect.DeepEqual(gotSeqs, wantSeqs) {
		t.Fatalf("readNewestBounded seqs = %v, want %v (ascending-scan equivalence)", gotSeqs, wantSeqs)
	}
}

func TestReadNewestBoundedTruncatesWhenBudgetExhausted(t *testing.T) {
	dir, sizes := seedThreeArchives(t)
	path := filepath.Join(dir, "events.jsonl")
	ctx := context.Background()

	// Budget fits exactly the newest archive ([5,6]) alone: opening the
	// middle archive ([3,4]) on top of it must exceed any positive size.
	got, truncated, reachedSeq, err := readNewestBounded(ctx, path, Filter{}, 10, sizes[2])
	if err != nil {
		t.Fatalf("readNewestBounded: %v", err)
	}
	if !truncated {
		t.Fatalf("truncated = false, want true (budget only covers the newest archive)")
	}
	if want := uint64(4 + 1); reachedSeq != want {
		t.Errorf("reachedSeq = %d, want %d (LastSeq+1 of the last archive NOT opened)", reachedSeq, want)
	}
	if gotSeqs := seqsOf(got); !reflect.DeepEqual(gotSeqs, []uint64{5, 6}) {
		t.Fatalf("readNewestBounded seqs = %v, want [5 6] (only the newest archive)", gotSeqs)
	}
}

func TestReadNewestBoundedResumeCursorHasNoGapOrOverlap(t *testing.T) {
	dir, sizes := seedThreeArchives(t)
	path := filepath.Join(dir, "events.jsonl")
	ctx := context.Background()

	first, truncated, reachedSeq, err := readNewestBounded(ctx, path, Filter{}, 10, sizes[2])
	if err != nil {
		t.Fatalf("first readNewestBounded: %v", err)
	}
	if !truncated {
		t.Fatalf("first call: truncated = false, want true")
	}

	second, truncated2, _, err := readNewestBounded(ctx, path, Filter{BeforeSeq: reachedSeq}, 10, 1<<30)
	if err != nil {
		t.Fatalf("second readNewestBounded: %v", err)
	}
	if truncated2 {
		t.Errorf("second call: truncated = true, want false (generous budget)")
	}

	combined := append(seqsOf(second), seqsOf(first)...) //nolint:gocritic // test-local slice, no aliasing concern
	want, err := ReadFiltered(path, Filter{})
	if err != nil {
		t.Fatalf("ReadFiltered: %v", err)
	}
	if wantSeqs := seqsOf(want); !reflect.DeepEqual(combined, wantSeqs) {
		t.Fatalf("resumed combined seqs = %v, want %v (no gap, no duplicate)", combined, wantSeqs)
	}
}

func TestReadNewestBoundedWithinBudgetReportsNoTruncation(t *testing.T) {
	dir, sizes := seedThreeArchives(t)
	path := filepath.Join(dir, "events.jsonl")
	ctx := context.Background()

	total := sizes[0] + sizes[1] + sizes[2]
	got, truncated, reachedSeq, err := readNewestBounded(ctx, path, Filter{}, 10, total)
	if err != nil {
		t.Fatalf("readNewestBounded: %v", err)
	}
	if truncated {
		t.Errorf("truncated = true, want false (budget exactly covers every archive)")
	}
	if reachedSeq != 0 {
		t.Errorf("reachedSeq = %d, want 0", reachedSeq)
	}
	if gotSeqs := seqsOf(got); !reflect.DeepEqual(gotSeqs, []uint64{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("readNewestBounded seqs = %v, want [1 2 3 4 5 6]", gotSeqs)
	}
}

func TestReadNewestBoundedNeverSkipsFirstArchiveRegardlessOfBudget(t *testing.T) {
	dir, _ := seedThreeArchives(t)
	path := filepath.Join(dir, "events.jsonl")
	ctx := context.Background()

	// A budget far smaller than even one archive's compressed size must
	// still make forward progress on the first (newest) archive it
	// considers — otherwise a too-small budget wedges every page at
	// truncated=true with zero events forever.
	got, truncated, reachedSeq, err := readNewestBounded(ctx, path, Filter{}, 10, 1)
	if err != nil {
		t.Fatalf("readNewestBounded: %v", err)
	}
	if !truncated {
		t.Fatalf("truncated = false, want true (budget=1 byte cannot cover the older archives)")
	}
	if want := uint64(4 + 1); reachedSeq != want {
		t.Errorf("reachedSeq = %d, want %d", reachedSeq, want)
	}
	if gotSeqs := seqsOf(got); !reflect.DeepEqual(gotSeqs, []uint64{5, 6}) {
		t.Fatalf("readNewestBounded seqs = %v, want [5 6] (newest archive still scanned despite tiny budget)", gotSeqs)
	}
}

func TestReadNewestBoundedIncludesInFlightRotatingSegment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	ctx := context.Background()

	var stderr bytes.Buffer
	src := filepath.Join(dir, "src.jsonl")
	writeJSONLEvents(t, src, 1, 2)
	archive := filepath.Join(dir, formatArchiveBasename(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC), 1, 2))
	if err := gzipAndArchive(src, archive, &stderr); err != nil {
		t.Fatalf("gzipAndArchive: %v", err)
	}
	// In-flight rotation: gzip has not finished, so this window lives only
	// in the plain-JSONL rotating-* file.
	writeJSONLEvents(t, filepath.Join(dir, "events.jsonl.rotating-20260507T120500Z-seq-3-4"), 3, 4)

	got, truncated, _, err := readNewestBounded(ctx, path, Filter{}, 10, 1<<30)
	if err != nil {
		t.Fatalf("readNewestBounded: %v", err)
	}
	if truncated {
		t.Errorf("truncated = true, want false")
	}
	if gotSeqs := seqsOf(got); !reflect.DeepEqual(gotSeqs, []uint64{1, 2, 3, 4}) {
		t.Fatalf("readNewestBounded seqs = %v, want [1 2 3 4] (in-flight rotating segment included)", gotSeqs)
	}
}

func TestReadNewestBoundedRespectsFetchLimit(t *testing.T) {
	dir, _ := seedThreeArchives(t)
	path := filepath.Join(dir, "events.jsonl")
	ctx := context.Background()

	got, truncated, _, err := readNewestBounded(ctx, path, Filter{}, 1, 1<<30)
	if err != nil {
		t.Fatalf("readNewestBounded: %v", err)
	}
	if truncated {
		t.Errorf("truncated = true, want false (fetch satisfied, not budget-exhausted)")
	}
	if gotSeqs := seqsOf(got); !reflect.DeepEqual(gotSeqs, []uint64{6}) {
		t.Fatalf("readNewestBounded seqs = %v, want [6] (newest event, not oldest, within the partially-needed archive)", gotSeqs)
	}
}

func TestFileRecorderListNewestBoundedUsesConfiguredBudget(t *testing.T) {
	dir, sizes := seedThreeArchives(t)
	path := filepath.Join(dir, "events.jsonl")
	writeJSONLEvents(t, path, 7)

	r, err := NewFileRecorder(path, io.Discard, WithScanBudget(sizes[2]))
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	defer r.Close() //nolint:errcheck // best-effort cleanup

	got, truncated, reachedSeq, err := r.ListNewestBounded(context.Background(), Filter{}, 10)
	if err != nil {
		t.Fatalf("ListNewestBounded: %v", err)
	}
	if !truncated {
		t.Fatalf("truncated = false, want true (recorder configured with a budget covering only the newest archive)")
	}
	if want := uint64(4 + 1); reachedSeq != want {
		t.Errorf("reachedSeq = %d, want %d", reachedSeq, want)
	}
	if gotSeqs := seqsOf(got); !reflect.DeepEqual(gotSeqs, []uint64{5, 6}) {
		t.Fatalf("ListNewestBounded seqs = %v, want [5 6]", gotSeqs)
	}
}

func TestReadNewestBoundedCursorInsideArchiveClampsAndResumesWithoutOverlap(t *testing.T) {
	dir, _ := seedThreeArchives(t)
	path := filepath.Join(dir, "events.jsonl")
	ctx := context.Background()

	// BeforeSeq=4 lands inside the middle archive [3,4]; a 1-byte budget
	// admits only that first overlapping archive and truncates at [1,2].
	const beforeSeq = 4
	first, truncated, reachedSeq, err := readNewestBounded(ctx, path, Filter{BeforeSeq: beforeSeq}, 10, 1)
	if err != nil {
		t.Fatalf("first readNewestBounded: %v", err)
	}
	if !truncated {
		t.Fatalf("first call: truncated = false, want true")
	}
	if reachedSeq > beforeSeq {
		t.Fatalf("reachedSeq = %d, want <= BeforeSeq %d (resume must never move above the cursor)", reachedSeq, beforeSeq)
	}
	if gotSeqs := seqsOf(first); !reflect.DeepEqual(gotSeqs, []uint64{3}) {
		t.Fatalf("first call seqs = %v, want [3]", gotSeqs)
	}

	second, truncated2, _, err := readNewestBounded(ctx, path, Filter{BeforeSeq: reachedSeq}, 10, 1<<30)
	if err != nil {
		t.Fatalf("second readNewestBounded: %v", err)
	}
	if truncated2 {
		t.Errorf("second call: truncated = true, want false (generous budget)")
	}
	combined := append(seqsOf(second), seqsOf(first)...) //nolint:gocritic // test-local slice, no aliasing concern
	if !reflect.DeepEqual(combined, []uint64{1, 2, 3}) {
		t.Fatalf("resumed combined seqs = %v, want [1 2 3] (no gap, no overlap, nothing at or above BeforeSeq)", combined)
	}
}

func TestReadNewestBoundedSkipsSupplementalArchiveAboveCursor(t *testing.T) {
	dir, _ := seedThreeArchives(t)
	path := filepath.Join(dir, "events.jsonl")
	ctx := context.Background()

	// Promote archive [7,8] between the base archives snapshot and the
	// supplemental listing, so it reaches readNewestBounded only through the
	// supplemental tier — with a window wholly at or above the cursor.
	var stderr bytes.Buffer
	promoted := false
	previous := readRotationDir
	t.Cleanup(func() { readRotationDir = previous })
	readRotationDir = func(name string) ([]os.DirEntry, error) {
		if !promoted {
			promoted = true
			src := filepath.Join(dir, "late.jsonl")
			writeJSONLEvents(t, src, 7, 8)
			dest := filepath.Join(dir, formatArchiveBasename(time.Date(2026, 5, 7, 12, 15, 0, 0, time.UTC), 7, 8))
			if err := gzipAndArchive(src, dest, &stderr); err != nil {
				t.Fatalf("gzip late archive: %v", err)
			}
		}
		return previous(name)
	}

	got, truncated, reachedSeq, err := readNewestBounded(ctx, path, Filter{BeforeSeq: 5}, 10, 1)
	if err != nil {
		t.Fatalf("readNewestBounded: %v", err)
	}
	if !promoted {
		t.Fatal("supplemental listing hook never ran")
	}
	if !truncated {
		t.Fatalf("truncated = false, want true (1-byte budget cannot cover [1,2] after [3,4])")
	}
	// Had [7,8] spent the first-archive allowance, [3,4] would truncate
	// with zero rows and a resume boundary equal to the cursor — a wedge.
	if gotSeqs := seqsOf(got); !reflect.DeepEqual(gotSeqs, []uint64{3, 4}) {
		t.Fatalf("seqs = %v, want [3 4] (supplemental archive above the cursor must not use the first-archive allowance)", gotSeqs)
	}
	if reachedSeq != 3 {
		t.Fatalf("reachedSeq = %d, want 3", reachedSeq)
	}
}

// TestReadNewestBoundedSkipsRotatingTwinOfListedArchive pins that a
// .rotating-* file left behind in the crash window between archive rename and
// source removal is not read alongside its canonical archive: both hold the
// same seqs, so reading both would duplicate them on the page.
func TestReadNewestBoundedSkipsRotatingTwinOfListedArchive(t *testing.T) {
	dir, _ := seedThreeArchives(t)
	path := filepath.Join(dir, "events.jsonl")
	writeJSONLEvents(t, filepath.Join(dir, "events.jsonl.rotating-20260507T120500Z-seq-3-4"), 3, 4)

	got, truncated, _, err := readNewestBounded(context.Background(), path, Filter{}, 10, 1<<30)
	if err != nil {
		t.Fatalf("readNewestBounded: %v", err)
	}
	if truncated {
		t.Errorf("truncated = true, want false")
	}
	if gotSeqs := seqsOf(got); !reflect.DeepEqual(gotSeqs, []uint64{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("seqs = %v, want [1 2 3 4 5 6] (rotating twin of a listed archive must not duplicate its seqs)", gotSeqs)
	}
}

// TestReadNewestBoundedReadsSupplementalTwinOnce pins that an archive
// promoted between the two listings, whose rotating twin also still exists,
// reaches the page once: the twin and the archive share a seq window inside
// the supplemental tier.
func TestReadNewestBoundedReadsSupplementalTwinOnce(t *testing.T) {
	dir, _ := seedThreeArchives(t)
	path := filepath.Join(dir, "events.jsonl")

	var stderr bytes.Buffer
	promoted := false
	previous := readRotationDir
	t.Cleanup(func() { readRotationDir = previous })
	readRotationDir = func(name string) ([]os.DirEntry, error) {
		if !promoted {
			promoted = true
			src := filepath.Join(dir, "late.jsonl")
			writeJSONLEvents(t, src, 7, 8)
			dest := filepath.Join(dir, formatArchiveBasename(time.Date(2026, 5, 7, 12, 15, 0, 0, time.UTC), 7, 8))
			if err := gzipAndArchive(src, dest, &stderr); err != nil {
				t.Fatalf("gzip late archive: %v", err)
			}
			writeJSONLEvents(t, filepath.Join(dir, "events.jsonl.rotating-20260507T121500Z-seq-7-8"), 7, 8)
		}
		return previous(name)
	}

	got, truncated, _, err := readNewestBounded(context.Background(), path, Filter{}, 20, 1<<30)
	if err != nil {
		t.Fatalf("readNewestBounded: %v", err)
	}
	if !promoted {
		t.Fatal("supplemental listing hook never ran")
	}
	if truncated {
		t.Errorf("truncated = true, want false")
	}
	if gotSeqs := seqsOf(got); !reflect.DeepEqual(gotSeqs, []uint64{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Fatalf("seqs = %v, want [1 2 3 4 5 6 7 8] (supplemental archive and its rotating twin read once)", gotSeqs)
	}
}

// writeArchiveOf gzips one archive stamped ts that holds every seq from first
// through last into dir.
func writeArchiveOf(t *testing.T, dir string, ts time.Time, first, last uint64) {
	t.Helper()
	var seqs []uint64
	for seq := first; seq <= last; seq++ {
		seqs = append(seqs, seq)
	}
	src := filepath.Join(dir, fmt.Sprintf("src-%d-%d.jsonl", first, last))
	writeJSONLEvents(t, src, seqs...)
	var stderr bytes.Buffer
	if err := gzipAndArchive(src, filepath.Join(dir, formatArchiveBasename(ts, first, last)), &stderr); err != nil {
		t.Fatalf("gzip archive [%d,%d]: %v", first, last, err)
	}
}

// promoteArchivesBetweenListings arranges for each seq window in windows to be
// archived between readNewestBounded's base archives snapshot and its
// supplemental listing, so those archives reach the scan only through the
// supplemental tier. The hook fires once, on the first supplemental listing.
func promoteArchivesBetweenListings(t *testing.T, dir string, windows ...[2]uint64) {
	t.Helper()
	promoted := false
	previous := readRotationDir
	t.Cleanup(func() { readRotationDir = previous })
	t.Cleanup(func() {
		if !promoted {
			t.Error("supplemental listing hook never ran")
		}
	})
	readRotationDir = func(name string) ([]os.DirEntry, error) {
		if !promoted {
			promoted = true
			for i, w := range windows {
				writeArchiveOf(t, dir, time.Date(2026, 5, 7, 12, 15+i, 0, 0, time.UTC), w[0], w[1])
			}
		}
		return previous(name)
	}
}

// archiveEventsByActor gzips one archive holding a bead.created event for every
// seq in [first, last], attributed to actorOf(seq).
func archiveEventsByActor(t *testing.T, dir string, ts time.Time, first, last uint64, actorOf func(uint64) string) {
	t.Helper()
	var b bytes.Buffer
	for seq := first; seq <= last; seq++ {
		fmt.Fprintf(&b, `{"seq":%d,"type":%q,"actor":%q}`+"\n", seq, BeadCreated, actorOf(seq))
	}
	src := filepath.Join(dir, "by-actor.jsonl")
	if err := os.WriteFile(src, b.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", src, err)
	}
	var stderr bytes.Buffer
	dest := filepath.Join(dir, formatArchiveBasename(ts, first, last))
	if err := gzipAndArchive(src, dest, &stderr); err != nil {
		t.Fatalf("gzip archive [%d,%d]: %v", first, last, err)
	}
}

// TestReadNewestBoundedHonorsCanceledContext pins that a scan whose request was
// canceled stops before it opens another source and returns the context error
// with no events: the request context is the only thing that stops a scan a
// client has already abandoned.
func TestReadNewestBoundedHonorsCanceledContext(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("base archive", func(t *testing.T) {
		dir, _ := seedThreeArchives(t)
		got, truncated, _, err := readNewestBounded(canceled, filepath.Join(dir, "events.jsonl"), Filter{}, 10, 1<<30)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled (checked before a base archive is opened)", err)
		}
		if len(got) != 0 || truncated {
			t.Errorf("seqs = %v truncated = %v, want no events and no truncation from a canceled scan", seqsOf(got), truncated)
		}
	})

	t.Run("in-flight rotating segment", func(t *testing.T) {
		dir := t.TempDir()
		writeJSONLEvents(t, filepath.Join(dir, "events.jsonl.rotating-20260507T120500Z-seq-3-4"), 3, 4)
		got, truncated, _, err := readNewestBounded(canceled, filepath.Join(dir, "events.jsonl"), Filter{}, 10, 1<<30)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled (checked before a supplemental source is read)", err)
		}
		if len(got) != 0 || truncated {
			t.Errorf("seqs = %v truncated = %v, want no events and no truncation from a canceled scan", seqsOf(got), truncated)
		}
	})
}

// TestReadNewestBoundedAppliesNonSeqPredicatesToArchiveRows pins that the
// archive walk applies the whole filter to the rows it decodes, not only the
// seq window and type screen the archive reader already enforces: a GET
// /events?actor= request reaches this scan with an Actor predicate.
func TestReadNewestBoundedAppliesNonSeqPredicatesToArchiveRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	actorOf := func(seq uint64) string {
		if seq%2 == 0 {
			return "even"
		}
		return "odd"
	}
	archiveEventsByActor(t, dir, time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC), 1, 4, actorOf)
	archiveEventsByActor(t, dir, time.Date(2026, 5, 7, 12, 5, 0, 0, time.UTC), 5, 8, actorOf)

	got, truncated, _, err := readNewestBounded(context.Background(), path, Filter{Actor: "even"}, 10, 1<<30)
	if err != nil {
		t.Fatalf("readNewestBounded: %v", err)
	}
	if truncated {
		t.Errorf("truncated = true, want false")
	}
	if gotSeqs := seqsOf(got); !reflect.DeepEqual(gotSeqs, []uint64{2, 4, 6, 8}) {
		t.Fatalf("seqs = %v, want [2 4 6 8] (only the rows attributed to the filter's actor)", gotSeqs)
	}
}

// TestReadNewestBoundedDoesNotChargeInFlightSegmentAgainstBudget pins that only
// archives spend the byte budget: an in-flight rotating segment is plain JSONL,
// not an archive, so it neither counts against the budget nor uses up the
// first-archive allowance. The newest archive behind it is still the one the
// scan always admits, whatever the budget.
func TestReadNewestBoundedDoesNotChargeInFlightSegmentAgainstBudget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	writeArchiveOf(t, dir, time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC), 1, 2)
	writeJSONLEvents(t, filepath.Join(dir, "events.jsonl.rotating-20260507T120500Z-seq-3-4"), 3, 4)

	got, truncated, reachedSeq, err := readNewestBounded(context.Background(), path, Filter{}, 10, 1)
	if err != nil {
		t.Fatalf("readNewestBounded: %v", err)
	}
	if truncated {
		t.Errorf("truncated = true (reachedSeq %d), want false (the rotating segment spends no budget, so the archive behind it is the first one charged)", reachedSeq)
	}
	if gotSeqs := seqsOf(got); !reflect.DeepEqual(gotSeqs, []uint64{1, 2, 3, 4}) {
		t.Fatalf("seqs = %v, want [1 2 3 4]", gotSeqs)
	}
}

// TestReadNewestBoundedDropsSeqSharedByOverlappingSources pins that a seq held
// by two overlapping sources reaches the page once: the archive [1,3] and the
// in-flight segment [3,5] share seq 3, and neither window matches the other, so
// only the walk's descending-seq guard can drop the second copy.
func TestReadNewestBoundedDropsSeqSharedByOverlappingSources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	writeArchiveOf(t, dir, time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC), 1, 3)
	writeJSONLEvents(t, filepath.Join(dir, "events.jsonl.rotating-20260507T120500Z-seq-3-5"), 3, 4, 5)

	got, truncated, _, err := readNewestBounded(context.Background(), path, Filter{}, 10, 1<<30)
	if err != nil {
		t.Fatalf("readNewestBounded: %v", err)
	}
	if truncated {
		t.Errorf("truncated = true, want false")
	}
	if gotSeqs := seqsOf(got); !reflect.DeepEqual(gotSeqs, []uint64{1, 2, 3, 4, 5}) {
		t.Fatalf("seqs = %v, want [1 2 3 4 5] (seq 3 is held by both sources and must appear once)", gotSeqs)
	}
}

// TestReadNewestBoundedFullPageIsNotTruncatedByOlderArchives pins that the scan
// stops as soon as the page holds fetch matches: an older archive that would
// exceed the budget is never charged, so finding exactly fetch matches stays
// ordinary pagination (truncated=false), in the base archives and in the
// supplemental tier alike.
func TestReadNewestBoundedFullPageIsNotTruncatedByOlderArchives(t *testing.T) {
	t.Run("base archives", func(t *testing.T) {
		dir, sizes := seedThreeArchives(t)
		got, truncated, reachedSeq, err := readNewestBounded(context.Background(), filepath.Join(dir, "events.jsonl"), Filter{}, 2, sizes[2])
		if err != nil {
			t.Fatalf("readNewestBounded: %v", err)
		}
		if truncated || reachedSeq != 0 {
			t.Errorf("truncated = %v reachedSeq = %d, want false and 0 (the newest archive alone fills the page)", truncated, reachedSeq)
		}
		if gotSeqs := seqsOf(got); !reflect.DeepEqual(gotSeqs, []uint64{5, 6}) {
			t.Fatalf("seqs = %v, want [5 6]", gotSeqs)
		}
	})

	t.Run("supplemental archives", func(t *testing.T) {
		dir, _ := seedThreeArchives(t)
		promoteArchivesBetweenListings(t, dir, [2]uint64{7, 8}, [2]uint64{9, 10})
		got, truncated, reachedSeq, err := readNewestBounded(context.Background(), filepath.Join(dir, "events.jsonl"), Filter{}, 2, 1)
		if err != nil {
			t.Fatalf("readNewestBounded: %v", err)
		}
		if truncated || reachedSeq != 0 {
			t.Errorf("truncated = %v reachedSeq = %d, want false and 0 (the newest supplemental archive alone fills the page)", truncated, reachedSeq)
		}
		if gotSeqs := seqsOf(got); !reflect.DeepEqual(gotSeqs, []uint64{9, 10}) {
			t.Fatalf("seqs = %v, want [9 10]", gotSeqs)
		}
	})
}

// TestReadNewestBoundedSupplementalTruncationResumesWithoutGapOrOverlap pins the
// resume boundary of a budget stop inside the supplemental tier: a follow-up
// call with BeforeSeq set to reachedSeq continues immediately below the first
// archive the stopped call did not open.
func TestReadNewestBoundedSupplementalTruncationResumesWithoutGapOrOverlap(t *testing.T) {
	dir, _ := seedThreeArchives(t)
	path := filepath.Join(dir, "events.jsonl")
	promoteArchivesBetweenListings(t, dir, [2]uint64{7, 8}, [2]uint64{9, 10})

	first, truncated, reachedSeq, err := readNewestBounded(context.Background(), path, Filter{}, 10, 1)
	if err != nil {
		t.Fatalf("first readNewestBounded: %v", err)
	}
	if !truncated {
		t.Fatalf("first call: truncated = false, want true (a 1-byte budget cannot cover [7,8] after [9,10])")
	}
	if want := uint64(8 + 1); reachedSeq != want {
		t.Errorf("reachedSeq = %d, want %d (LastSeq+1 of the supplemental archive NOT opened)", reachedSeq, want)
	}
	if gotSeqs := seqsOf(first); !reflect.DeepEqual(gotSeqs, []uint64{9, 10}) {
		t.Fatalf("first call seqs = %v, want [9 10]", gotSeqs)
	}

	second, truncated2, _, err := readNewestBounded(context.Background(), path, Filter{BeforeSeq: reachedSeq}, 10, 1<<30)
	if err != nil {
		t.Fatalf("second readNewestBounded: %v", err)
	}
	if truncated2 {
		t.Errorf("second call: truncated = true, want false (generous budget)")
	}
	combined := append([]uint64{}, seqsOf(second)...)
	combined = append(combined, seqsOf(first)...)
	if want := []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}; !reflect.DeepEqual(combined, want) {
		t.Fatalf("resumed combined seqs = %v, want %v (no gap, no duplicate)", combined, want)
	}
}

// TestReadNewestBoundedSupplementalTwinOfReadSegmentSpendsNoBudget pins that an
// archive promoted between the two listings, whose rotating twin was already
// read, is skipped before the budget is charged. The pair [7,8] is walked
// rotating-first, which costs nothing; charging the archive as well would hand
// it the first-archive allowance and move the stop up from the boundary below
// the newest base archive.
func TestReadNewestBoundedSupplementalTwinOfReadSegmentSpendsNoBudget(t *testing.T) {
	dir, _ := seedThreeArchives(t)
	path := filepath.Join(dir, "events.jsonl")
	writeJSONLEvents(t, filepath.Join(dir, "events.jsonl.rotating-20260507T121500Z-seq-7-8"), 7, 8)
	promoteArchivesBetweenListings(t, dir, [2]uint64{7, 8})

	got, truncated, reachedSeq, err := readNewestBounded(context.Background(), path, Filter{}, 10, 1)
	if err != nil {
		t.Fatalf("readNewestBounded: %v", err)
	}
	if !truncated {
		t.Fatalf("truncated = false, want true (a 1-byte budget cannot cover [3,4] after [5,6])")
	}
	if want := uint64(4 + 1); reachedSeq != want {
		t.Errorf("reachedSeq = %d, want %d (the promoted archive's twin was read once and free, so [5,6] held the allowance)", reachedSeq, want)
	}
	if gotSeqs := seqsOf(got); !reflect.DeepEqual(gotSeqs, []uint64{5, 6, 7, 8}) {
		t.Fatalf("seqs = %v, want [5 6 7 8]", gotSeqs)
	}
}

// TestReadNewestBoundedClampsTruncationResumeToTheCursor pins the clamp on a
// budget stop: when the archive the budget refuses to open reaches past the
// incoming cursor, the resume boundary is the cursor itself, never that
// archive's LastSeq+1 above it, so pagination cannot move backwards. Only an
// overlapping window ([4,8] here) can put an unopened archive past the cursor.
func TestReadNewestBoundedClampsTruncationResumeToTheCursor(t *testing.T) {
	dir, _ := seedThreeArchives(t)
	path := filepath.Join(dir, "events.jsonl")
	writeArchiveOf(t, dir, time.Date(2026, 5, 7, 12, 15, 0, 0, time.UTC), 4, 8)

	const beforeSeq = 6
	got, truncated, reachedSeq, err := readNewestBounded(context.Background(), path, Filter{BeforeSeq: beforeSeq}, 10, 1)
	if err != nil {
		t.Fatalf("readNewestBounded: %v", err)
	}
	if !truncated {
		t.Fatalf("truncated = false, want true (a 1-byte budget cannot cover [4,8] after [5,6])")
	}
	if reachedSeq != beforeSeq {
		t.Errorf("reachedSeq = %d, want %d (the unopened archive ends at 8, but the resume boundary never moves above the cursor)", reachedSeq, beforeSeq)
	}
	if gotSeqs := seqsOf(got); !reflect.DeepEqual(gotSeqs, []uint64{5}) {
		t.Fatalf("seqs = %v, want [5] (only the newest archive's rows below the cursor)", gotSeqs)
	}
}

// TestFileRecorderListNewestBoundedDefaultsToTheDefaultBudget pins that a
// recorder built without WithScanBudget scans with the default budget, not a
// zero one: the three archives are far below 128 MiB, so one call walks them
// all.
func TestFileRecorderListNewestBoundedDefaultsToTheDefaultBudget(t *testing.T) {
	dir, _ := seedThreeArchives(t)
	path := filepath.Join(dir, "events.jsonl")
	writeJSONLEvents(t, path, 7)

	r, err := NewFileRecorder(path, io.Discard)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	defer r.Close() //nolint:errcheck // best-effort cleanup

	got, truncated, reachedSeq, err := r.ListNewestBounded(context.Background(), Filter{}, 10)
	if err != nil {
		t.Fatalf("ListNewestBounded: %v", err)
	}
	if truncated || reachedSeq != 0 {
		t.Errorf("truncated = %v reachedSeq = %d, want false and 0 (the default budget covers every archive)", truncated, reachedSeq)
	}
	if gotSeqs := seqsOf(got); !reflect.DeepEqual(gotSeqs, []uint64{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("ListNewestBounded seqs = %v, want [1 2 3 4 5 6] (archives only; the active file is the tail's job)", gotSeqs)
	}
}
