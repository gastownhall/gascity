package events

import (
	"bytes"
	"context"
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
