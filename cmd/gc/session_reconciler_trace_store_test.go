package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"
)

// failingTraceSegmentFile writes only the first cut(p) bytes of the next
// Write and then fails it, the way a write that hits ENOSPC midway does.
// truncateErr, when set, also makes the rollback truncate fail.
type failingTraceSegmentFile struct {
	*os.File
	cut         func(p []byte) int
	truncateErr error
	failed      bool
}

func (f *failingTraceSegmentFile) Write(p []byte) (int, error) {
	if f.failed {
		return f.File.Write(p)
	}
	f.failed = true
	n := min(f.cut(p), len(p))
	written, err := f.File.Write(p[:n])
	if err != nil {
		return written, err
	}
	return written, syscall.ENOSPC
}

func (f *failingTraceSegmentFile) Truncate(size int64) error {
	if f.truncateErr != nil {
		return f.truncateErr
	}
	return f.File.Truncate(size)
}

// injectTraceWriteFailure makes the store's next segment write stop after
// cut(p) bytes and fail. truncateErr, when set, also makes rolling the
// segment back fail.
func injectTraceWriteFailure(t *testing.T, store *SessionReconcilerTraceStore, cut func(p []byte) int, truncateErr error) {
	t.Helper()
	file, ok := store.currentFile.(*os.File)
	if !ok {
		t.Fatalf("current segment file = %T, want *os.File", store.currentFile)
	}
	store.currentFile = &failingTraceSegmentFile{File: file, cut: cut, truncateErr: truncateErr}
}

func traceStoreTestRecord(traceID string, ts time.Time) SessionReconcilerTraceRecord {
	return SessionReconcilerTraceRecord{
		TraceSchemaVersion: sessionReconcilerTraceSchemaVersion,
		TraceID:            traceID,
		TickID:             traceID + "-tick",
		RecordType:         TraceRecordDecision,
		Template:           "repo/worker",
		SessionName:        "worker-1",
		SiteCode:           TraceSiteReconcilerWakeDecision,
		TraceMode:          TraceModeDetail,
		TraceSource:        TraceSourceManual,
		Ts:                 ts,
	}
}

func traceStoreTestRoot(cityDir string) string {
	return filepath.Join(cityDir, ".gc", "runtime", sessionReconcilerTraceRootDir)
}

func traceStoreTestSegments(t *testing.T, root string) []string {
	t.Helper()
	segments, err := filepath.Glob(filepath.Join(root, sessionReconcilerTraceSegments, "*", "*", "*", "*.jsonl"))
	if err != nil {
		t.Fatalf("glob segments: %v", err)
	}
	sort.Strings(segments)
	return segments
}

func appendTraceStoreTestBatch(t *testing.T, store *SessionReconcilerTraceStore, traceID string, count int, offset time.Duration, tier TraceDurabilityTier) {
	t.Helper()
	now := time.Now().UTC().Add(offset)
	records := make([]SessionReconcilerTraceRecord, count)
	for i := range records {
		records[i] = traceStoreTestRecord(traceID, now)
	}
	if err := store.AppendBatch(records, tier); err != nil {
		t.Fatalf("append batch %s: %v", traceID, err)
	}
}

// TestTraceStoreFailedWriteRollsBackPartialBatch pins what a failed batch
// write leaves behind (issue #7954): the write error surfaces, the segment is
// byte-identical to its state before the failed call whatever the cut point,
// and the seqs the failed batch would have used are not consumed.
func TestTraceStoreFailedWriteRollsBackPartialBatch(t *testing.T) {
	tiers := []struct {
		name string
		tier TraceDurabilityTier
	}{
		{"durable", TraceDurabilityDurable},
		{"metadata", TraceDurabilityMetadata},
	}
	for _, tier := range tiers {
		t.Run(tier.name, func(t *testing.T) {
			cuts := []struct {
				name string
				cut  func([]byte) int
			}{
				{"tear-inside-record", func(p []byte) int { return bytes.IndexByte(p, '\n') + 1 + 20 }},
				{"tear-after-record-line", func(p []byte) int { return bytes.IndexByte(p, '\n') + 1 }},
				{"commit-line-without-newline", func(p []byte) int { return len(p) - 1 }},
				{"no-bytes", func([]byte) int { return 0 }},
			}
			for _, tc := range cuts {
				t.Run(tc.name, func(t *testing.T) {
					cityDir := t.TempDir()
					store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
					if err != nil {
						t.Fatalf("new store: %v", err)
					}
					defer store.Close() //nolint:errcheck
					appendTraceStoreTestBatch(t, store, "cycle-a", 2, 0, tier.tier)
					segments := traceStoreTestSegments(t, traceStoreTestRoot(cityDir))
					if len(segments) != 1 {
						t.Fatalf("segments = %d, want 1", len(segments))
					}
					before, err := os.ReadFile(segments[0])
					if err != nil {
						t.Fatalf("read segment: %v", err)
					}
					seqBefore, err := store.LatestSeq()
					if err != nil {
						t.Fatalf("LatestSeq: %v", err)
					}
					injectTraceWriteFailure(t, store, tc.cut, nil)
					failed := make([]SessionReconcilerTraceRecord, 2)
					for i := range failed {
						failed[i] = traceStoreTestRecord("cycle-b", time.Now().UTC().Add(time.Second))
					}
					if err := store.AppendBatch(failed, tier.tier); !errors.Is(err, syscall.ENOSPC) {
						t.Fatalf("append batch b error = %v, want ENOSPC", err)
					}
					after, err := os.ReadFile(segments[0])
					if err != nil {
						t.Fatalf("read segment after failed write: %v", err)
					}
					if !bytes.Equal(before, after) {
						t.Fatalf("segment changed after the failed write:\nbefore:\n%s\nafter:\n%s", before, after)
					}
					seqAfter, err := store.LatestSeq()
					if err != nil {
						t.Fatalf("LatestSeq after failure: %v", err)
					}
					if seqAfter != seqBefore {
						t.Fatalf("LatestSeq = %d, want %d (the failed batch's seqs are not consumed)", seqAfter, seqBefore)
					}
				})
			}
		})
	}

	// The first batch of a brand-new segment rolls back to zero bytes, so a
	// full disk does not leave a segment file per tick.
	t.Run("first-batch-of-fresh-segment", func(t *testing.T) {
		cityDir := t.TempDir()
		root := traceStoreTestRoot(cityDir)
		store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
		if err != nil {
			t.Fatalf("new store: %v", err)
		}
		defer store.Close() //nolint:errcheck
		now := time.Now().UTC()
		segmentPath := filepath.Join(traceDayDir(filepath.Join(root, sessionReconcilerTraceSegments), now), traceSegmentFileName(1))
		if err := os.MkdirAll(filepath.Dir(segmentPath), sessionReconcilerTraceOwnerDirPerm); err != nil {
			t.Fatalf("mkdir segment dir: %v", err)
		}
		if err := store.openSegment(segmentPath, 0, 0); err != nil {
			t.Fatalf("openSegment: %v", err)
		}
		injectTraceWriteFailure(t, store, func([]byte) int { return 40 }, nil)
		if err := store.AppendBatch([]SessionReconcilerTraceRecord{traceStoreTestRecord("cycle-b", now)}, TraceDurabilityDurable); !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("append batch b error = %v, want ENOSPC", err)
		}
		segments := traceStoreTestSegments(t, root)
		if len(segments) != 1 {
			t.Fatalf("segments = %d, want 1", len(segments))
		}
		data, err := os.ReadFile(segments[0])
		if err != nil {
			t.Fatalf("read segment: %v", err)
		}
		if len(data) != 0 {
			t.Fatalf("segment holds %d bytes after a rolled-back first batch, want 0:\n%s", len(data), data)
		}
		seq, err := store.LatestSeq()
		if err != nil {
			t.Fatalf("LatestSeq: %v", err)
		}
		if seq != 0 {
			t.Fatalf("LatestSeq = %d, want 0", seq)
		}
	})
}
