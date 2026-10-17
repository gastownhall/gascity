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

func traceStoreTestSegments(t *testing.T, root string) []string {
	t.Helper()
	segments, err := filepath.Glob(filepath.Join(root, sessionReconcilerTraceSegments, "*", "*", "*", "*.jsonl"))
	if err != nil {
		t.Fatalf("glob segments: %v", err)
	}
	sort.Strings(segments)
	return segments
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

func TestTraceStoreFailedWriteRollsBackPartialBatch(t *testing.T) {
	cityDir := t.TempDir()
	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close() //nolint:errcheck

	now := time.Now().UTC()
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{traceStoreTestRecord("cycle-a", now)}, TraceDurabilityDurable); err != nil {
		t.Fatalf("append batch a: %v", err)
	}
	injectTraceWriteFailure(t, store, func([]byte) int { return 40 }, nil)
	failed := []SessionReconcilerTraceRecord{
		traceStoreTestRecord("cycle-b", now.Add(time.Second)),
		traceStoreTestRecord("cycle-b", now.Add(time.Second)),
	}
	if err := store.AppendBatch(failed, TraceDurabilityDurable); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("append batch b error = %v, want ENOSPC", err)
	}
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{traceStoreTestRecord("cycle-c", now.Add(2*time.Second))}, TraceDurabilityDurable); err != nil {
		t.Fatalf("append batch c: %v", err)
	}

	root := traceStoreTestRoot(cityDir)
	segments := traceStoreTestSegments(t, root)
	if len(segments) != 1 {
		t.Fatalf("segments = %d, want 1 (rolled-back segment keeps being used)", len(segments))
	}
	data, err := os.ReadFile(segments[0])
	if err != nil {
		t.Fatalf("read segment: %v", err)
	}
	if bytes.Contains(data, []byte("cycle-b")) {
		t.Fatalf("segment still holds bytes of the failed batch:\n%s", data)
	}
	records, err := ReadTraceRecords(root, TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("records = %d, want 4 (two batches of record+commit)", len(records))
	}
	assertTraceSeqsUnique(t, records)
}

func TestTraceStoreFailedRollbackRotatesWithoutSeqReuse(t *testing.T) {
	cityDir := t.TempDir()
	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close() //nolint:errcheck

	now := time.Now().UTC()
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{traceStoreTestRecord("cycle-a", now)}, TraceDurabilityDurable); err != nil {
		t.Fatalf("append batch a: %v", err)
	}
	seqBefore, err := store.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq: %v", err)
	}
	// Fail after the first record line and partway into the second, so a
	// complete-but-uncommitted line and a torn line stay on disk.
	failed := []SessionReconcilerTraceRecord{
		traceStoreTestRecord("cycle-b", now.Add(time.Second)),
		traceStoreTestRecord("cycle-b", now.Add(time.Second)),
	}
	cutInSecondLine := func(p []byte) int { return bytes.IndexByte(p, '\n') + 1 + 20 }
	injectTraceWriteFailure(t, store, cutInSecondLine, errors.New("truncate refused"))
	if err := store.AppendBatch(failed, TraceDurabilityDurable); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("append batch b error = %v, want ENOSPC", err)
	}
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{traceStoreTestRecord("cycle-c", now.Add(2*time.Second))}, TraceDurabilityDurable); err != nil {
		t.Fatalf("append batch c: %v", err)
	}

	root := traceStoreTestRoot(cityDir)
	segments := traceStoreTestSegments(t, root)
	if len(segments) != 2 {
		t.Fatalf("segments = %d, want 2 (next batch rotates off the torn segment)", len(segments))
	}
	torn, err := os.ReadFile(segments[0])
	if err != nil {
		t.Fatalf("read torn segment: %v", err)
	}
	if !bytes.Contains(torn, []byte("cycle-b")) || bytes.HasSuffix(torn, []byte("\n")) {
		t.Fatalf("first segment should keep the torn batch-b bytes without a trailing newline:\n%s", torn)
	}
	fresh, err := os.ReadFile(segments[1])
	if err != nil {
		t.Fatalf("read fresh segment: %v", err)
	}
	if bytes.Contains(fresh, []byte("cycle-b")) {
		t.Fatalf("fresh segment holds bytes of the failed batch:\n%s", fresh)
	}

	records, err := ReadTraceRecords(root, TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("records = %d, want 4 (batches a and c)", len(records))
	}
	for _, rec := range records {
		if rec.TraceID == "cycle-c" && rec.Seq <= seqBefore+uint64(len(failed))+1 {
			t.Fatalf("batch c record seq %d reuses a seq the failed batch left on disk (seq before = %d)", rec.Seq, seqBefore)
		}
	}
	assertTraceSeqsUnique(t, records)
}

func TestTraceStoreOpenSegmentRotatesOffTornTail(t *testing.T) {
	cityDir := t.TempDir()
	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	now := time.Now().UTC()
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{traceStoreTestRecord("cycle-a", now)}, TraceDurabilityDurable); err != nil {
		t.Fatalf("append batch a: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	root := traceStoreTestRoot(cityDir)
	segments := traceStoreTestSegments(t, root)
	if len(segments) != 1 {
		t.Fatalf("segments = %d, want 1", len(segments))
	}
	tornPath := segments[0]
	// A crash mid-write leaves a torn record with no trailing newline.
	f, err := os.OpenFile(tornPath, os.O_APPEND|os.O_WRONLY, sessionReconcilerTraceOwnerFilePerm)
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	if _, err := f.WriteString(`{"trace_schema_version":1,"seq":3,"trace_id":"cycle-torn","ts":"2026-1`); err != nil {
		f.Close() //nolint:errcheck
		t.Fatalf("write torn tail: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close segment: %v", err)
	}
	tornBefore, err := os.ReadFile(tornPath)
	if err != nil {
		t.Fatalf("read torn segment: %v", err)
	}

	reopened, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close() //nolint:errcheck
	// Reopen the torn segment for append, as a store that keeps writing
	// into its current segment does.
	if err := reopened.openSegment(tornPath); err != nil {
		t.Fatalf("openSegment: %v", err)
	}
	if err := reopened.AppendBatch([]SessionReconcilerTraceRecord{traceStoreTestRecord("cycle-b", now.Add(time.Second))}, TraceDurabilityDurable); err != nil {
		t.Fatalf("append batch b: %v", err)
	}

	tornAfter, err := os.ReadFile(tornPath)
	if err != nil {
		t.Fatalf("read torn segment after append: %v", err)
	}
	if !bytes.Equal(tornBefore, tornAfter) {
		t.Fatalf("batch b was appended onto the torn segment:\n%s", tornAfter)
	}
	segments = traceStoreTestSegments(t, root)
	if len(segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(segments))
	}
	records, err := ReadTraceRecords(root, TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("records = %d, want 4 (batches a and b)", len(records))
	}
	assertTraceSeqsUnique(t, records)
}

func TestTraceStoreRestartAfterTornTailAppendsCleanly(t *testing.T) {
	cityDir := t.TempDir()
	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	now := time.Now().UTC()
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{traceStoreTestRecord("cycle-a", now)}, TraceDurabilityDurable); err != nil {
		t.Fatalf("append batch a: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	root := traceStoreTestRoot(cityDir)
	segments := traceStoreTestSegments(t, root)
	if len(segments) != 1 {
		t.Fatalf("segments = %d, want 1", len(segments))
	}
	f, err := os.OpenFile(segments[0], os.O_APPEND|os.O_WRONLY, sessionReconcilerTraceOwnerFilePerm)
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	if _, err := f.WriteString(`{"trace_schema_version":1,"seq":3,"trace_id":"cycle-torn","ts":"2026-1`); err != nil {
		f.Close() //nolint:errcheck
		t.Fatalf("write torn tail: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close segment: %v", err)
	}

	reopened, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close() //nolint:errcheck
	if err := reopened.AppendBatch([]SessionReconcilerTraceRecord{traceStoreTestRecord("cycle-b", now.Add(time.Second))}, TraceDurabilityDurable); err != nil {
		t.Fatalf("append batch b: %v", err)
	}
	quarantined, err := filepath.Glob(filepath.Join(root, sessionReconcilerTraceQuarantine, "*"))
	if err != nil {
		t.Fatalf("glob quarantine: %v", err)
	}
	if len(quarantined) != 0 {
		t.Fatalf("quarantined = %v, want none for a torn tail", quarantined)
	}
	records, err := ReadTraceRecords(root, TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("records = %d, want 4 (batches a and b)", len(records))
	}
	assertTraceSeqsUnique(t, records)
}

// TestTraceReaderSkipsMidFileTornBatch rebuilds the on-disk shape a failed
// write used to leave: an uncommitted batch whose last line is cut off and
// glued to the first line of the next batch, followed by good batches.
func TestTraceReaderSkipsMidFileTornBatch(t *testing.T) {
	cityDir := t.TempDir()
	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	now := time.Now().UTC()
	for i, id := range []string{"cycle-a", "cycle-b", "cycle-c", "cycle-d"} {
		batch := []SessionReconcilerTraceRecord{
			traceStoreTestRecord(id, now.Add(time.Duration(i)*time.Second)),
			traceStoreTestRecord(id, now.Add(time.Duration(i)*time.Second)),
		}
		if err := store.AppendBatch(batch, TraceDurabilityDurable); err != nil {
			t.Fatalf("append batch %s: %v", id, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	root := traceStoreTestRoot(cityDir)
	segments := traceStoreTestSegments(t, root)
	if len(segments) != 1 {
		t.Fatalf("segments = %d, want 1", len(segments))
	}
	data, err := os.ReadFile(segments[0])
	if err != nil {
		t.Fatalf("read segment: %v", err)
	}
	lines := bytes.SplitAfter(data, []byte("\n"))
	if len(lines) != 13 || len(lines[12]) != 0 {
		t.Fatalf("segment lines = %d, want 4 batches of 3 lines", len(lines)-1)
	}
	// Batch b (lines 3-5) failed after its first record and 20 bytes of its
	// second; batch c (lines 6-8) was then appended onto the torn bytes.
	var poisoned []byte
	poisoned = append(poisoned, bytes.Join(lines[0:3], nil)...)
	poisoned = append(poisoned, lines[3]...)
	poisoned = append(poisoned, lines[4][:20]...)
	poisoned = append(poisoned, bytes.Join(lines[6:12], nil)...)
	if err := os.WriteFile(segments[0], poisoned, sessionReconcilerTraceOwnerFilePerm); err != nil {
		t.Fatalf("write poisoned segment: %v", err)
	}

	records, err := ReadTraceRecords(root, TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	got := make(map[string]int)
	for _, rec := range records {
		got[rec.TraceID]++
	}
	if got["cycle-a"] != 3 || got["cycle-d"] != 3 {
		t.Fatalf("records by trace = %v, want cycle-a and cycle-d complete (3 each)", got)
	}
	if got["cycle-b"] != 0 || got["cycle-c"] != 0 {
		t.Fatalf("records by trace = %v, want torn cycle-b and glued cycle-c dropped", got)
	}

	maxSeq, ok, err := scanTraceSegment(segments[0])
	if err != nil || !ok {
		t.Fatalf("scanTraceSegment = %d, %v, %v; want the segment to scan cleanly", maxSeq, ok, err)
	}
	if maxSeq != 12 {
		t.Fatalf("scanTraceSegment maxSeq = %d, want 12", maxSeq)
	}

	reopened, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close() //nolint:errcheck
	quarantined, err := filepath.Glob(filepath.Join(root, sessionReconcilerTraceQuarantine, "*"))
	if err != nil {
		t.Fatalf("glob quarantine: %v", err)
	}
	if len(quarantined) != 0 {
		t.Fatalf("quarantined = %v, want none", quarantined)
	}
}

func TestTraceReaderKeepsIntactBatchAfterTornLine(t *testing.T) {
	cityDir := t.TempDir()
	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	now := time.Now().UTC()
	for i, id := range []string{"cycle-a", "cycle-b"} {
		if err := store.AppendBatch([]SessionReconcilerTraceRecord{traceStoreTestRecord(id, now.Add(time.Duration(i)*time.Second))}, TraceDurabilityDurable); err != nil {
			t.Fatalf("append batch %s: %v", id, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	root := traceStoreTestRoot(cityDir)
	segments := traceStoreTestSegments(t, root)
	data, err := os.ReadFile(segments[0])
	if err != nil {
		t.Fatalf("read segment: %v", err)
	}
	lines := bytes.SplitAfter(data, []byte("\n"))
	// A torn line that ends in its own newline, then an intact batch b.
	var shaped []byte
	shaped = append(shaped, bytes.Join(lines[0:2], nil)...)
	shaped = append(shaped, []byte("{\"trace_schema_version\":1,\"seq\":3,\"ts\":\"2026-1\n")...)
	shaped = append(shaped, bytes.Join(lines[2:4], nil)...)
	if err := os.WriteFile(segments[0], shaped, sessionReconcilerTraceOwnerFilePerm); err != nil {
		t.Fatalf("write segment: %v", err)
	}
	records, err := ReadTraceRecords(root, TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("records = %d, want 4 (batches a and b)", len(records))
	}
}

func TestTraceReaderRejectsCorruptCommittedBatch(t *testing.T) {
	cityDir := t.TempDir()
	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{traceStoreTestRecord("cycle-a", time.Now().UTC())}, TraceDurabilityDurable); err != nil {
		t.Fatalf("append batch: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	root := traceStoreTestRoot(cityDir)
	segments := traceStoreTestSegments(t, root)
	data, err := os.ReadFile(segments[0])
	if err != nil {
		t.Fatalf("read segment: %v", err)
	}
	// Flip the worker name inside the committed record: it still parses, but
	// the batch CRC no longer matches.
	corrupt := bytes.Replace(data, []byte(`"worker-1"`), []byte(`"worker-2"`), 1)
	if bytes.Equal(corrupt, data) {
		t.Fatal("did not find the record field to corrupt")
	}
	if err := os.WriteFile(segments[0], corrupt, sessionReconcilerTraceOwnerFilePerm); err != nil {
		t.Fatalf("write segment: %v", err)
	}
	if _, err := ReadTraceRecords(root, TraceFilter{}); err == nil {
		t.Fatal("ReadTraceRecords succeeded on a committed batch with a bad CRC, want an error")
	}
}
