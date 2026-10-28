package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// TestTraceStoreNextBatchAfterFailedWrite pins the batch appended after a
// rolled-back failure (issue #7954): it goes to the same segment, starts on
// its own line with the next unused seq, and leaves no trace of the failed
// batch.
func TestTraceStoreNextBatchAfterFailedWrite(t *testing.T) {
	t.Run("after-committed-batch", func(t *testing.T) {
		cityDir := t.TempDir()
		root := traceStoreTestRoot(cityDir)
		store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
		if err != nil {
			t.Fatalf("new store: %v", err)
		}
		defer store.Close() //nolint:errcheck
		appendTraceStoreTestBatch(t, store, "cycle-a", 1, 0, TraceDurabilityDurable)
		preSeq, err := store.LatestSeq()
		if err != nil {
			t.Fatalf("LatestSeq: %v", err)
		}
		injectTraceWriteFailure(t, store, func([]byte) int { return 40 }, nil)
		failed := []SessionReconcilerTraceRecord{
			traceStoreTestRecord("cycle-b", time.Now().UTC().Add(time.Second)),
			traceStoreTestRecord("cycle-b", time.Now().UTC().Add(time.Second)),
		}
		if err := store.AppendBatch(failed, TraceDurabilityDurable); !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("append batch b error = %v, want ENOSPC", err)
		}
		appendTraceStoreTestBatch(t, store, "cycle-c", 1, 2*time.Second, TraceDurabilityDurable)

		segments := traceStoreTestSegments(t, root)
		if len(segments) != 1 {
			t.Fatalf("segments = %d, want 1 (the rolled-back segment keeps being used)", len(segments))
		}
		records, err := ReadTraceRecords(root, TraceFilter{})
		if err != nil {
			t.Fatalf("ReadTraceRecords: %v", err)
		}
		counts := traceTestRecordCountByTraceID(records)
		if counts["cycle-a"] != 2 || counts["cycle-c"] != 2 || counts["cycle-b"] != 0 {
			t.Fatalf("records by trace = %v, want cycle-a and cycle-c committed, no cycle-b", counts)
		}
		commit := traceTestCommitRecord(t, records, "cycle-c")
		if commit.FirstSeq != preSeq+1 {
			t.Fatalf("batch c first_seq = %d, want %d (the failed batch's seqs are reused safely)", commit.FirstSeq, preSeq+1)
		}
		assertTraceSeqsUnique(t, records)
	})

	t.Run("on-fresh-segment", func(t *testing.T) {
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
		appendTraceStoreTestBatch(t, store, "cycle-c", 1, time.Second, TraceDurabilityDurable)

		segments := traceStoreTestSegments(t, root)
		if len(segments) != 1 {
			t.Fatalf("segments = %d, want 1 (the truncated fresh segment keeps being used)", len(segments))
		}
		records, err := ReadTraceRecords(root, TraceFilter{})
		if err != nil {
			t.Fatalf("ReadTraceRecords: %v", err)
		}
		counts := traceTestRecordCountByTraceID(records)
		if counts["cycle-c"] != 2 || counts["cycle-b"] != 0 {
			t.Fatalf("records by trace = %v, want only cycle-c", counts)
		}
		commit := traceTestCommitRecord(t, records, "cycle-c")
		if commit.FirstSeq != 1 {
			t.Fatalf("batch c first_seq = %d, want 1", commit.FirstSeq)
		}
		assertTraceSeqsUnique(t, records)
	})
}

// TestTraceStoreFailedRollbackAbandonsSegment pins the rollback half where
// the segment cannot be cut back (issue #7954): the next batch starts a
// segment with a higher index, the failed batch's seqs are retired, and the
// committed batches around the torn tail stay readable.
func TestTraceStoreFailedRollbackAbandonsSegment(t *testing.T) {
	// Fail after the first record line and partway into the second, so a
	// complete-but-uncommitted line and a torn line stay on disk, and make
	// the rollback truncate fail too.
	failTornBatch := func(t *testing.T, store *SessionReconcilerTraceStore) error {
		injectTraceWriteFailure(t, store, func(p []byte) int { return bytes.IndexByte(p, '\n') + 1 + 20 }, errors.New("truncate refused"))
		failed := make([]SessionReconcilerTraceRecord, 2)
		now := time.Now().UTC().Add(time.Second)
		for i := range failed {
			failed[i] = traceStoreTestRecord("cycle-b", now)
		}
		return store.AppendBatch(failed, TraceDurabilityDurable)
	}
	assertRecovered := func(t *testing.T, cityDir string) []SessionReconcilerTraceRecord {
		t.Helper()
		root := traceStoreTestRoot(cityDir)
		segments := traceStoreTestSegments(t, root)
		if len(segments) != 2 {
			t.Fatalf("segments = %d, want 2 (the next batch lands in a new segment)", len(segments))
		}
		torn, err := os.ReadFile(segments[0])
		if err != nil {
			t.Fatalf("read torn segment: %v", err)
		}
		if !bytes.Contains(torn, []byte("cycle-b")) || bytes.HasSuffix(torn, []byte("\n")) {
			t.Fatalf("first segment should keep the torn batch bytes without a trailing newline:\n%s", torn)
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
		counts := traceTestRecordCountByTraceID(records)
		if counts["cycle-a"] != 2 || counts["cycle-c"] != 2 || counts["cycle-b"] != 0 {
			t.Fatalf("records by trace = %v, want cycle-a and cycle-c committed, no cycle-b", counts)
		}
		// The failed batch would have committed at seq 5 (records 3 and 4).
		commit := traceTestCommitRecord(t, records, "cycle-c")
		if commit.FirstSeq <= 5 {
			t.Fatalf("batch c first seq = %d, want > 5 (greater than the failed batch's commit seq)", commit.FirstSeq)
		}
		assertTraceSeqsUnique(t, records)
		return records
	}

	t.Run("same-store", func(t *testing.T) {
		cityDir := t.TempDir()
		store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
		if err != nil {
			t.Fatalf("new store: %v", err)
		}
		defer store.Close() //nolint:errcheck
		appendTraceStoreTestBatch(t, store, "cycle-a", 1, 0, TraceDurabilityDurable)
		if err := failTornBatch(t, store); !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("append batch b error = %v, want ENOSPC", err)
		}
		appendTraceStoreTestBatch(t, store, "cycle-c", 1, 2*time.Second, TraceDurabilityDurable)
		assertRecovered(t, cityDir)
	})

	t.Run("reopened-store", func(t *testing.T) {
		cityDir := t.TempDir()
		store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
		if err != nil {
			t.Fatalf("new store: %v", err)
		}
		appendTraceStoreTestBatch(t, store, "cycle-a", 1, 0, TraceDurabilityDurable)
		if err := failTornBatch(t, store); !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("append batch b error = %v, want ENOSPC", err)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
		reopened, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
		if err != nil {
			t.Fatalf("reopen store: %v", err)
		}
		defer reopened.Close() //nolint:errcheck
		if quarantined := traceTestQuarantined(t, traceStoreTestRoot(cityDir)); len(quarantined) != 0 {
			t.Fatalf("quarantined = %v, want none", quarantined)
		}
		seq, err := reopened.LatestSeq()
		if err != nil {
			t.Fatalf("LatestSeq after reopen: %v", err)
		}
		if seq != 5 {
			t.Fatalf("LatestSeq after reopen = %d, want 5 (the failed batch's commit seq is retired)", seq)
		}
		appendTraceStoreTestBatch(t, reopened, "cycle-c", 1, 2*time.Second, TraceDurabilityDurable)
		assertRecovered(t, cityDir)
	})
}

func traceTestQuarantined(t *testing.T, root string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(root, sessionReconcilerTraceQuarantine, "*"))
	if err != nil {
		t.Fatalf("glob quarantine: %v", err)
	}
	return found
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

func traceTestRecordCountByTraceID(records []SessionReconcilerTraceRecord) map[string]int {
	counts := make(map[string]int)
	for _, rec := range records {
		counts[rec.TraceID]++
	}
	return counts
}

func traceTestCommitRecord(t *testing.T, records []SessionReconcilerTraceRecord, traceID string) SessionReconcilerTraceRecord {
	t.Helper()
	for _, rec := range records {
		if rec.RecordType == TraceRecordBatchCommit && rec.TraceID == traceID {
			return rec
		}
	}
	t.Fatalf("no batch commit for %s among %d records", traceID, len(records))
	return SessionReconcilerTraceRecord{}
}

// buildTraceSegmentBatch lays down one committed batch the way AppendBatch
// does: count records with seqs from firstSeq, a CRC32 over the trimmed record
// lines, and a trailing batch_commit record.
func buildTraceSegmentBatch(t *testing.T, traceID string, firstSeq uint64, count int, ts time.Time) []byte {
	t.Helper()
	var buf bytes.Buffer
	var crc uint32
	for i := 0; i < count; i++ {
		rec := traceStoreTestRecord(traceID, ts)
		rec.Seq = firstSeq + uint64(i)
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal record %s seq %d: %v", traceID, rec.Seq, err)
		}
		line = append(line, '\n')
		buf.Write(line)
		crc = crc32.Update(crc, crc32.IEEETable, bytes.TrimSpace(line))
	}
	commit := SessionReconcilerTraceRecord{
		TraceSchemaVersion: sessionReconcilerTraceSchemaVersion,
		TraceID:            traceID,
		TickID:             traceID + "-tick",
		RecordType:         TraceRecordBatchCommit,
		Ts:                 ts,
		Seq:                firstSeq + uint64(count),
		FirstSeq:           firstSeq,
		LastSeq:            firstSeq + uint64(count) - 1,
		RecordCount:        count,
		BatchCRC32:         crc,
		TraceMode:          TraceModeBaseline,
		TraceSource:        TraceSourceAlwaysOn,
	}
	line, err := json.Marshal(commit)
	if err != nil {
		t.Fatalf("marshal commit %s: %v", traceID, err)
	}
	buf.Write(append(line, '\n'))
	return buf.Bytes()
}

// writeTraceTestSegment writes raw segment bytes as segment-000001 under the
// trace root.
func writeTraceTestSegment(t *testing.T, root string, contents ...[]byte) string {
	t.Helper()
	dayDir := filepath.Join(root, sessionReconcilerTraceSegments, "2026", "10", "15")
	if err := os.MkdirAll(dayDir, sessionReconcilerTraceOwnerDirPerm); err != nil {
		t.Fatalf("mkdir segment dir: %v", err)
	}
	path := filepath.Join(dayDir, traceSegmentFileName(1))
	if err := os.WriteFile(path, bytes.Join(contents, nil), sessionReconcilerTraceOwnerFilePerm); err != nil {
		t.Fatalf("write segment: %v", err)
	}
	return path
}

// traceTornBatchSegment builds the on-disk shape a failed batch write left
// behind (issue #7954): a committed batch, an uncommitted record cut off
// inside its ts field, the next batch glued onto the torn bytes and reusing
// its seqs, and committed batches after it when withTrailingBatch is set.
func traceTornBatchSegment(t *testing.T, root string, glueBatchCount int, withTrailingBatch bool) string {
	t.Helper()
	now := time.Now().UTC()
	var segment []byte
	segment = append(segment, buildTraceSegmentBatch(t, "cycle-a", 1, 2, now)...)
	// The uncommitted batch would have written seqs 4 and 5; its write was
	// cut off inside the first record.
	segment = append(segment, []byte(`{"trace_schema_version":1,"seq":4,"trace_id":"cycle-x","ts":"2026-1`)...)
	segment = append(segment, buildTraceSegmentBatch(t, "cycle-b", 4, glueBatchCount, now.Add(time.Second))...)
	if withTrailingBatch {
		firstSeq := uint64(4 + glueBatchCount + 1)
		segment = append(segment, buildTraceSegmentBatch(t, "cycle-c", firstSeq, 2, now.Add(2*time.Second))...)
	}
	return writeTraceTestSegment(t, root, segment)
}

// TestTraceReaderSkipsTornBatchGluedToNextBatch reads the segment shape
// issue #7954 left behind: an uncommitted batch cut off mid-record with the
// next batch glued onto the torn bytes. The reader must serve the committed
// batches around the glued line instead of failing the whole query.
func TestTraceReaderSkipsTornBatchGluedToNextBatch(t *testing.T) {
	t.Run("report-shape", func(t *testing.T) {
		cityDir := t.TempDir()
		root := traceStoreTestRoot(cityDir)
		traceTornBatchSegment(t, root, 2, true)

		records, err := ReadTraceRecords(root, TraceFilter{})
		if err != nil {
			t.Fatalf("ReadTraceRecords: %v", err)
		}
		counts := traceTestRecordCountByTraceID(records)
		if counts["cycle-a"] != 3 || counts["cycle-c"] != 3 || counts["cycle-b"] != 0 {
			t.Fatalf("records by trace = %v, want cycle-a and cycle-c committed, no cycle-b", counts)
		}
		assertTraceSeqsUnique(t, records)

		// The SeqAfter pre-scan reads the same segment and must not fail
		// either. cycle-a's commit holds seq 3, so only the trailing
		// committed batch is above the bound.
		after, err := ReadTraceRecords(root, TraceFilter{SeqAfter: 3})
		if err != nil {
			t.Fatalf("ReadTraceRecords with SeqAfter: %v", err)
		}
		counts = traceTestRecordCountByTraceID(after)
		if len(counts) != 1 || counts["cycle-c"] != 3 {
			t.Fatalf("records by trace above seq 3 = %v, want only cycle-c", counts)
		}
	})

	t.Run("glued-single-record-batch", func(t *testing.T) {
		cityDir := t.TempDir()
		root := traceStoreTestRoot(cityDir)
		path := traceTornBatchSegment(t, root, 1, false)

		records, err := ReadTraceRecords(root, TraceFilter{})
		if err != nil {
			t.Fatalf("ReadTraceRecords: %v", err)
		}
		counts := traceTestRecordCountByTraceID(records)
		if len(counts) != 1 || counts["cycle-a"] != 3 {
			t.Fatalf("records by trace = %v, want only cycle-a", counts)
		}
		// The glued batch's commit sees 0 buffered records against
		// record_count=1 while resynchronizing; only verified commits count
		// into the segment's max seq.
		maxSeq, ok, err := scanTraceSegment(path)
		if err != nil || !ok {
			t.Fatalf("scanTraceSegment = %d, %v, %v; want the segment to scan cleanly", maxSeq, ok, err)
		}
		if maxSeq != 3 {
			t.Fatalf("scanTraceSegment maxSeq = %d, want 3 (cycle-a's commit seq; verified commits only)", maxSeq)
		}
	})
}

// TestTraceRecoveryKeepsTornSegment reopens a store over a segment shaped like
// issue #7954 with no head matching its size. The glued line is a torn write,
// not silent corruption: the segment stays, its committed batches stay
// readable, and the next append goes to a fresh segment with the next seq.
func TestTraceRecoveryKeepsTornSegment(t *testing.T) {
	cityDir := t.TempDir()
	root := traceStoreTestRoot(cityDir)
	traceTornBatchSegment(t, root, 2, true)

	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close() //nolint:errcheck

	if quarantined := traceTestQuarantined(t, root); len(quarantined) != 0 {
		t.Fatalf("quarantined = %v, want none for a torn write", quarantined)
	}
	segments := traceStoreTestSegments(t, root)
	if len(segments) != 1 {
		t.Fatalf("segments = %d, want the poisoned segment kept", len(segments))
	}
	seq, err := store.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq: %v", err)
	}
	if seq != 9 {
		t.Fatalf("LatestSeq = %d, want 9 (the highest verified commit seq)", seq)
	}

	appendTraceStoreTestBatch(t, store, "cycle-z", 1, time.Second, TraceDurabilityDurable)
	if quarantined := traceTestQuarantined(t, root); len(quarantined) != 0 {
		t.Fatalf("quarantined = %v, want none after the append", quarantined)
	}
	segments = traceStoreTestSegments(t, root)
	if len(segments) != 2 {
		t.Fatalf("segments after append = %d, want a fresh segment", len(segments))
	}
	records, err := ReadTraceRecords(root, TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	counts := traceTestRecordCountByTraceID(records)
	if counts["cycle-a"] != 3 || counts["cycle-c"] != 3 || counts["cycle-z"] != 2 {
		t.Fatalf("records by trace = %v, want cycle-a, cycle-c and cycle-z", counts)
	}
	commit := traceTestCommitRecord(t, records, "cycle-z")
	if commit.FirstSeq != 10 {
		t.Fatalf("cycle-z first_seq = %d, want 10 (LatestSeq before the append, plus one)", commit.FirstSeq)
	}
	assertTraceSeqsUnique(t, records)
}

// TestTraceStoreRestartAfterTornTailUsesFreshSegment pins the crash path at
// issue #7954's core: a torn tail left by a crash mid-write is tolerated by
// the reader, quarantined nowhere, and the restarted store appends to a new
// segment with its own seqs.
func TestTraceStoreRestartAfterTornTailUsesFreshSegment(t *testing.T) {
	cityDir := t.TempDir()
	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	appendTraceStoreTestBatch(t, store, "cycle-a", 1, 0, TraceDurabilityDurable)
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
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
	appendTraceStoreTestBatch(t, reopened, "cycle-b", 1, time.Second, TraceDurabilityDurable)

	if quarantined := traceTestQuarantined(t, root); len(quarantined) != 0 {
		t.Fatalf("quarantined = %v, want none for a torn tail", quarantined)
	}
	segments = traceStoreTestSegments(t, root)
	if len(segments) != 2 {
		t.Fatalf("segments after restart = %d, want a fresh segment", len(segments))
	}
	records, err := ReadTraceRecords(root, TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	counts := traceTestRecordCountByTraceID(records)
	if counts["cycle-a"] != 2 || counts["cycle-b"] != 2 {
		t.Fatalf("records by trace = %v, want cycle-a and cycle-b", counts)
	}
	assertTraceSeqsUnique(t, records)
}

// TestTraceReaderRejectsCorruptCommittedBatch keeps the reader's corruption
// guard: a batch commit whose records do not verify its count or CRC is an
// error. A resync window opened by a torn line is closed by the next
// verified commit, so corruption in a later committed batch still surfaces.
func TestTraceReaderRejectsCorruptCommittedBatch(t *testing.T) {
	assertCRCError := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("ReadTraceRecords succeeded on a committed batch with a bad CRC, want an error")
		}
		if !strings.Contains(err.Error(), "crc32") {
			t.Fatalf("error = %v, want a crc32 mismatch", err)
		}
	}

	t.Run("crc-mismatch", func(t *testing.T) {
		cityDir := t.TempDir()
		root := traceStoreTestRoot(cityDir)
		batch := buildTraceSegmentBatch(t, "cycle-a", 1, 1, time.Now().UTC())
		// Flip the worker name inside the committed record: it still parses,
		// but the batch CRC no longer matches.
		corrupt := bytes.Replace(batch, []byte(`"worker-1"`), []byte(`"worker-2"`), 1)
		if bytes.Equal(corrupt, batch) {
			t.Fatal("did not find the record field to corrupt")
		}
		writeTraceTestSegment(t, root, corrupt)
		_, err := ReadTraceRecords(root, TraceFilter{})
		assertCRCError(t, err)
	})

	t.Run("crc-mismatch-after-resync-window-closed", func(t *testing.T) {
		cityDir := t.TempDir()
		root := traceStoreTestRoot(cityDir)
		now := time.Now().UTC()
		var segment []byte
		segment = append(segment, buildTraceSegmentBatch(t, "cycle-a", 1, 1, now)...)
		// A torn line that ends in its own newline: an unparseable line
		// mid-file, not a tail.
		segment = append(segment, []byte(`{"trace_schema_version":1,"seq":3,"ts":"2026-1`+"\n")...)
		segment = append(segment, buildTraceSegmentBatch(t, "cycle-b", 3, 1, now.Add(time.Second))...)
		corrupt := buildTraceSegmentBatch(t, "cycle-c", 5, 1, now.Add(2*time.Second))
		corrupt = bytes.Replace(corrupt, []byte(`"worker-1"`), []byte(`"worker-2"`), 1)
		writeTraceTestSegment(t, root, segment, corrupt)
		_, err := ReadTraceRecords(root, TraceFilter{})
		assertCRCError(t, err)
	})
}
