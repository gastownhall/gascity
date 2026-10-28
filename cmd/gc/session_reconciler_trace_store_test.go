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
// truncateErr, when set, makes Truncate fail too.
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

// injectTraceWriteFailure makes the store's next segment write stop after
// cut(p) bytes and fail with ENOSPC.
func injectTraceWriteFailure(t *testing.T, store *SessionReconcilerTraceStore, cut func(p []byte) int, truncateErr error) {
	t.Helper()
	file, ok := store.currentFile.(*os.File)
	if !ok {
		t.Fatalf("current segment file = %T, want *os.File", store.currentFile)
	}
	store.currentFile = &failingTraceSegmentFile{File: file, cut: cut, truncateErr: truncateErr}
}

var traceFailedWriteCuts = []struct {
	name string
	cut  func(p []byte) int
}{
	{"torn inside a record", func([]byte) int { return 40 }},
	{"torn after a complete record line", func(p []byte) int { return bytes.IndexByte(p, '\n') + 1 }},
	{"commit line without its newline", func(p []byte) int { return len(p) - 1 }},
	{"nothing written", func([]byte) int { return 0 }},
}

// failedTraceAppend is the state a failed, rolled-back AppendBatch leaves.
type failedTraceAppend struct {
	store     *SessionReconcilerTraceStore
	root      string
	segPath   string
	before    []byte
	seqBefore uint64
	appendErr error
}

// failTraceAppendOnce appends batch cycle-a (unless fresh), then makes the
// next AppendBatch (two cycle-b records) fail after cut(p) bytes. With fresh
// set, the failure is the first write into a brand-new segment.
func failTraceAppendOnce(t *testing.T, fresh bool, durability TraceDurabilityTier, cut func(p []byte) int) failedTraceAppend {
	t.Helper()
	cityDir := t.TempDir()
	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if fresh {
		path, err := store.currentSegmentPath(now)
		if err != nil {
			t.Fatalf("currentSegmentPath: %v", err)
		}
		if err := store.openSegment(path, -1, -1); err != nil {
			t.Fatalf("openSegment: %v", err)
		}
	} else if err := store.AppendBatch([]SessionReconcilerTraceRecord{traceStoreTestRecord("cycle-a", now)}, durability); err != nil {
		t.Fatalf("append batch a: %v", err)
	}
	segPath := store.currentPath
	before, err := os.ReadFile(segPath)
	if err != nil {
		t.Fatalf("read segment: %v", err)
	}
	seqBefore, err := store.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq: %v", err)
	}
	injectTraceWriteFailure(t, store, cut, nil)
	failed := []SessionReconcilerTraceRecord{
		traceStoreTestRecord("cycle-b", now.Add(time.Second)),
		traceStoreTestRecord("cycle-b", now.Add(time.Second)),
	}
	appendErr := store.AppendBatch(failed, durability)
	return failedTraceAppend{
		store:     store,
		root:      traceStoreTestRoot(cityDir),
		segPath:   segPath,
		before:    before,
		seqBefore: seqBefore,
		appendErr: appendErr,
	}
}

// forEachFailedTraceAppend runs fn for every cut point, durability tier, and
// for both a segment holding a committed batch and a brand-new segment.
func forEachFailedTraceAppend(t *testing.T, fn func(t *testing.T, fresh bool, durability TraceDurabilityTier, cut func(p []byte) int)) {
	for _, tc := range traceFailedWriteCuts {
		for _, durability := range []TraceDurabilityTier{TraceDurabilityDurable, TraceDurabilityMetadata} {
			for _, fresh := range []bool{false, true} {
				name := tc.name + "/" + string(durability) + "/existing segment"
				if fresh {
					name = tc.name + "/" + string(durability) + "/fresh segment"
				}
				t.Run(name, func(t *testing.T) {
					fn(t, fresh, durability, tc.cut)
				})
			}
		}
	}
}

func TestTraceStoreFailedWriteRollsBackPartialBatch(t *testing.T) {
	forEachFailedTraceAppend(t, func(t *testing.T, fresh bool, durability TraceDurabilityTier, cut func(p []byte) int) {
		got := failTraceAppendOnce(t, fresh, durability, cut)
		if !errors.Is(got.appendErr, syscall.ENOSPC) {
			t.Fatalf("append batch b error = %v, want ENOSPC", got.appendErr)
		}
		after, err := os.ReadFile(got.segPath)
		if err != nil {
			t.Fatalf("read segment: %v", err)
		}
		if !bytes.Equal(after, got.before) {
			t.Fatalf("segment after failed write:\n%s\nwant it unchanged:\n%s", after, got.before)
		}
		seqAfter, err := got.store.LatestSeq()
		if err != nil {
			t.Fatalf("LatestSeq: %v", err)
		}
		if seqAfter != got.seqBefore {
			t.Fatalf("LatestSeq after failed write = %d, want %d", seqAfter, got.seqBefore)
		}
	})
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

// assertTraceSeqsUnique fails when two records share a seq.
func assertTraceSeqsUnique(t *testing.T, records []SessionReconcilerTraceRecord) {
	t.Helper()
	seen := make(map[uint64]SessionReconcilerTraceRecord)
	for _, rec := range records {
		if prev, ok := seen[rec.Seq]; ok {
			t.Fatalf("seq %d reused: %s %s and %s %s", rec.Seq, prev.TraceID, prev.RecordType, rec.TraceID, rec.RecordType)
		}
		seen[rec.Seq] = rec
	}
}

// traceRecordCounts counts the records of each trace id.
func traceRecordCounts(records []SessionReconcilerTraceRecord) map[string]int {
	counts := make(map[string]int)
	for _, rec := range records {
		counts[rec.TraceID]++
	}
	return counts
}

func TestTraceStoreAppendAfterRolledBackWriteContinuesSegment(t *testing.T) {
	forEachFailedTraceAppend(t, func(t *testing.T, fresh bool, durability TraceDurabilityTier, cut func(p []byte) int) {
		got := failTraceAppendOnce(t, fresh, durability, cut)
		if got.appendErr == nil {
			t.Fatal("append batch b: want a write error, got nil")
		}
		next := traceStoreTestRecord("cycle-c", time.Now().UTC().Add(2*time.Second))
		if err := got.store.AppendBatch([]SessionReconcilerTraceRecord{next}, durability); err != nil {
			t.Fatalf("append batch c: %v", err)
		}

		segments := traceStoreTestSegments(t, got.root)
		if len(segments) != 1 || segments[0] != got.segPath {
			t.Fatalf("segments = %v, want only %s (the rolled-back segment keeps being used)", segments, got.segPath)
		}
		after, err := os.ReadFile(got.segPath)
		if err != nil {
			t.Fatalf("read segment: %v", err)
		}
		if !bytes.HasPrefix(after, got.before) || !bytes.HasPrefix(after[len(got.before):], []byte(`{"`)) {
			t.Fatalf("batch c does not start on its own line after the committed bytes:\n%s", after)
		}

		records, err := ReadTraceRecords(got.root, TraceFilter{})
		if err != nil {
			t.Fatalf("ReadTraceRecords: %v", err)
		}
		assertTraceSeqsUnique(t, records)
		counts := traceRecordCounts(records)
		wantA := 2
		if fresh {
			wantA = 0
		}
		if counts["cycle-a"] != wantA || counts["cycle-c"] != 2 || counts["cycle-b"] != 0 {
			t.Fatalf("records by trace = %v, want cycle-a=%d cycle-c=2 and no cycle-b", counts, wantA)
		}
		for _, rec := range records {
			if rec.TraceID == "cycle-c" && rec.RecordType == TraceRecordBatchCommit && rec.FirstSeq != got.seqBefore+1 {
				t.Fatalf("batch c first_seq = %d, want %d", rec.FirstSeq, got.seqBefore+1)
			}
		}
	})
}
